package env

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ViBiOh/kmux/pkg/client"
	"github.com/ViBiOh/kmux/pkg/output"
	"github.com/ViBiOh/kmux/pkg/resource"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	envLabels      = regexp.MustCompile(`(?m)metadata\.labels\[["']?(.*?)["']?\]`)
	envAnnotations = regexp.MustCompile(`(?m)metadata\.annotations\[["']?(.*?)["']?\]`)
)

type EnvGetter struct {
	containerRegexp *regexp.Regexp
	resourceType    string
	resourceName    string
}

func NewEnvGetter(resourceType, resourceName string) EnvGetter {
	return EnvGetter{
		resourceType: resourceType,
		resourceName: resourceName,
	}
}

func (eg EnvGetter) WithContainerRegexp(containerRegexp *regexp.Regexp) EnvGetter {
	eg.containerRegexp = containerRegexp

	return eg
}

func (eg EnvGetter) Get(ctx context.Context, kube client.Kube) error {
	podSpec, err := resource.GetPodSpec(ctx, kube, eg.resourceType, eg.resourceName)
	if err != nil {
		return err
	}

	pods, err := resource.ListPods(ctx, kube, eg.resourceType, eg.resourceName)
	if err != nil {
		return err
	}

	for _, container := range append(podSpec.InitContainers, podSpec.Containers...) {
		if !resource.IsContainedSelected(container, eg.containerRegexp) {
			continue
		}

		values := getEnv(ctx, kube, container, getMostLivePod(pods))

		if len(values) > 0 {
			containerOutput := &strings.Builder{}

			for source, content := range values {
				fmt.Fprintf(containerOutput, "# %s\n", source)

				for key, value := range content {
					fmt.Fprintf(containerOutput, "%s=%s\n", key, value)
				}
			}

			kube.Info("%s %s", output.Green.Sprintf("[%s]", container.Name), containerOutput.String())
		}
	}

	return nil
}

func getMostLivePod(pods []v1.Pod) v1.Pod {
	for _, pod := range pods {
		if pod.Status.Phase == v1.PodRunning {
			return pod
		}
	}

	for _, pod := range pods {
		if pod.Status.Phase == v1.PodSucceeded {
			return pod
		}
	}

	for _, pod := range pods {
		if pod.Status.Phase == v1.PodFailed {
			return pod
		}
	}

	for _, pod := range pods {
		if pod.Status.Phase == v1.PodPending {
			return pod
		}
	}

	for _, pod := range pods {
		if pod.Status.Phase == v1.PodUnknown {
			return pod
		}
	}

	return v1.Pod{}
}

func getEnv(ctx context.Context, kube client.Kube, container v1.Container, pod v1.Pod) map[string]map[string]string {
	output := make(map[string]map[string]string)

	configMaps, secrets := getEnvDependencies(ctx, kube, container)

	for _, envFrom := range container.EnvFrom {
		var key string
		var content map[string]string

		if envFrom.ConfigMapRef != nil {
			key, content = getEnvFromSource(configMaps, "configmap", envFrom.Prefix, envFrom.ConfigMapRef.Name, envFrom.ConfigMapRef.Optional)
		} else if envFrom.SecretRef != nil {
			key, content = getEnvFromSource(secrets, "secret", envFrom.Prefix, envFrom.SecretRef.Name, envFrom.SecretRef.Optional)
		}

		output[key] = content
	}

	if len(container.Env) > 0 {
		inline := make(map[string]string)

		for _, env := range container.Env {
			inline[env.Name] = getInlineEnv(pod, env, configMaps, secrets)
		}

		output["inline"] = inline
	}

	return output
}

func getEnvDependencies(ctx context.Context, kube client.Kube, container v1.Container) (map[string]map[string]string, map[string]map[string]string) {
	configMaps := make(map[string]map[string]string)
	secrets := make(map[string]map[string]string)

	for _, env := range container.Env {
		if env.ValueFrom != nil {
			if env.ValueFrom.ConfigMapKeyRef != nil {
				configMaps[env.ValueFrom.ConfigMapKeyRef.LocalObjectReference.Name] = nil
			} else if env.ValueFrom.SecretKeyRef != nil {
				secrets[env.ValueFrom.SecretKeyRef.LocalObjectReference.Name] = nil
			}
		}
	}

	for _, envFrom := range container.EnvFrom {
		if envFrom.ConfigMapRef != nil {
			configMaps[envFrom.ConfigMapRef.LocalObjectReference.Name] = nil
		} else if envFrom.SecretRef != nil {
			secrets[envFrom.SecretRef.LocalObjectReference.Name] = nil
		}
	}

	for name := range configMaps {
		configMap, err := kube.CoreV1().ConfigMaps(kube.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			kube.Err("getting configmap `%s`: %s", name, err)
			continue
		}

		configMaps[name] = configMap.Data
	}

	for name := range secrets {
		secret, err := kube.CoreV1().Secrets(kube.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			kube.Err("getting secret `%s`: %s", name, err)
			continue
		}

		secrets[name] = make(map[string]string)

		for key, value := range secret.Data {
			secrets[name][key] = string(value)
		}
	}

	return configMaps, secrets
}

func getEnvFromSource(storage map[string]map[string]string, kind, prefix, name string, optional *bool) (string, map[string]string) {
	content := make(map[string]string)
	keyName := fmt.Sprintf("%s %s", kind, name)

	values, ok := storage[name]
	if !ok {
		if optional != nil && !*optional {
			content["error"] = fmt.Sprintf("<%s not optional and not found>", kind)

			return keyName, content
		}
	}

	for key, value := range values {
		content[prefix+key] = value
	}

	return keyName, content
}

func getInlineEnv(pod v1.Pod, envVar v1.EnvVar, configMaps, secrets map[string]map[string]string) string {
	if len(envVar.Value) > 0 {
		return envVar.Value
	}

	return getValueFrom(pod, envVar, configMaps, secrets)
}

func getValueFrom(pod v1.Pod, envVar v1.EnvVar, configMaps, secrets map[string]map[string]string) string {
	if envVar.ValueFrom.ConfigMapKeyRef != nil {
		return getValueFromRef(configMaps, "configmap", envVar.ValueFrom.ConfigMapKeyRef.Name, envVar.ValueFrom.ConfigMapKeyRef.Key, envVar.ValueFrom.ConfigMapKeyRef.Optional)
	}

	if envVar.ValueFrom.SecretKeyRef != nil {
		return getValueFromRef(secrets, "secret", envVar.ValueFrom.SecretKeyRef.Name, envVar.ValueFrom.SecretKeyRef.Key, envVar.ValueFrom.SecretKeyRef.Optional)
	}

	if envVar.ValueFrom.FieldRef != nil {
		return getEnvFieldRef(pod, *envVar.ValueFrom.FieldRef)
	}

	if envVar.ValueFrom.ResourceFieldRef != nil {
		return getEnvResourceRef(pod.Spec, *envVar.ValueFrom.ResourceFieldRef)
	}

	return ""
}

func getValueFromRef(storage map[string]map[string]string, kind, name, key string, optional *bool) string {
	values, ok := storage[name]
	if !ok {
		if optional != nil && !*optional {
			return fmt.Sprintf("<%s `%s` not optional and not found>", kind, name)
		}
	}

	return values[key]
}

func getEnvFieldRef(pod v1.Pod, field v1.ObjectFieldSelector) string {
	if matches := envLabels.FindAllStringSubmatch(field.FieldPath, -1); len(matches) > 0 {
		return matches[0][1]
	}

	if matches := envAnnotations.FindAllStringSubmatch(field.FieldPath, -1); len(matches) > 0 {
		return matches[0][1]
	}

	switch field.FieldPath {
	case "metadata.name":
		return pod.GetName()

	case "metadata.namespace":
		return pod.GetNamespace()

	case "spec.nodeName":
		return pod.Spec.NodeName

	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName

	case "status.hostIP":
		return pod.Status.HostIP

	case "status.podIP":
		return pod.Status.PodIP

	case "status.podIPs":
		output := make([]string, len(pod.Status.PodIPs))
		for index, ip := range pod.Status.PodIPs {
			output[index] = ip.IP
		}

		return strings.Join(output, ",")

	default:
		return fmt.Sprintf("<`%s` field ref not implemented>", field.FieldPath)
	}
}

func getEnvResourceRef(pod v1.PodSpec, resource v1.ResourceFieldSelector) string {
	var container v1.Container

	for _, container = range pod.Containers {
		if resource.ContainerName == container.Name {
			break
		}
	}

	switch resource.Resource {
	case "limits.cpu":
		return container.Resources.Limits.Cpu().String()
	case "limits.memory":
		return container.Resources.Limits.Memory().String()
	case "limits.ephemeral-storage":
		return container.Resources.Limits.StorageEphemeral().String()
	case "requests.cpu":
		return container.Resources.Requests.Cpu().String()
	case "requests.memory":
		return container.Resources.Requests.Memory().String()
	case "requests.ephemeral-storage":
		return container.Resources.Requests.StorageEphemeral().String()
	default:
		return ""
	}
}