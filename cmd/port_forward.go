package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ViBiOh/kmux/pkg/client"
	"github.com/ViBiOh/kmux/pkg/concurrent"
	"github.com/ViBiOh/kmux/pkg/resource"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

var portForwardCmd = &cobra.Command{
	Use:   "port-forward <resource_type> <resource_name> <local_port> <remote_port>",
	Short: "Port forward to a ressources",
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return []string{
				"daemonsets",
				"deployments",
				"pods",
				"statefulsets",
			}, cobra.ShellCompDirectiveNoFileComp
		}

		if len(args) == 1 {
			lister, err := resource.ListerFor(args[0])
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}

			clients, err = getKubernetesClient(strings.Split(viper.GetString("context"), ","))
			if err != nil {
				return nil, cobra.ShellCompDirectiveError
			}

			return getCommonObjects(viper.GetString("namespace"), lister), cobra.ShellCompDirectiveNoFileComp
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	Args: cobra.ExactValidArgs(4),
	RunE: func(cmd *cobra.Command, args []string) error {
		resourceType := args[0]
		resourceName := args[1]
		rawLocalPort := args[2]
		rawRemotePort := args[3]

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		localPort, err := strconv.ParseUint(rawLocalPort, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid local port: %s", rawLocalPort)
		}

		remotePort, err := strconv.ParseUint(rawRemotePort, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid remote port: %s", rawRemotePort)
		}

		go func() {
			waitForEnd(syscall.SIGINT, syscall.SIGTERM)
			cancel()
		}()

		clients.Execute(func(kube client.Kube) error {
			podWatcher, err := resource.WatchPods(ctx, kube, resourceType, resourceName, dryRun)
			if err != nil {
				return err
			}

			defer podWatcher.Stop()

			var activeForwarding sync.Map

			forwarding := concurrent.NewSimple()

			for event := range podWatcher.ResultChan() {
				pod, ok := event.Object.(*v1.Pod)
				if !ok {
					continue
				}

				forwardStop, ok := activeForwarding.Load(pod.UID)
				if event.Type == watch.Deleted || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
					if ok {
						close(forwardStop.(chan struct{}))
					}

					continue
				}

				if ok || pod.Status.Phase != v1.PodRunning {
					continue
				}

				localPort += 1
				handleForwardPod(kube, &activeForwarding, forwarding, *pod, localPort, remotePort)
			}

			activeForwarding.Range(func(key, value any) bool {
				close(value.(chan struct{}))
				return true
			})

			forwarding.Wait()

			return nil
		})

		return nil
	},
}

func handleForwardPod(kube client.Kube, activeForwarding *sync.Map, forwarding *concurrent.Simple, pod v1.Pod, localPort, remotePort uint64) {
	stopChan := make(chan struct{})
	activeForwarding.Store(pod.UID, stopChan)

	forwarding.Go(func() {
		defer activeForwarding.Delete(pod.UID)

		kube.Info("Starting port-forward 127.0.0.1:%d => %s:%d", localPort, pod.Name, remotePort)
		defer kube.Warn("Port-forward 127.0.0.1:%d => %s:%d ended.", localPort, pod.Name, remotePort)

		if err := listenPortForward(kube, pod, stopChan, localPort, remotePort); err != nil {
			kube.Err("Port-forward for %s failed: %s", pod.Name, err)
		}
	})
}

func listenPortForward(kube client.Kube, pod v1.Pod, stopChan chan struct{}, localPort, podPort uint64) error {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", pod.Namespace, pod.Name)
	hostIP := strings.TrimPrefix(kube.Config.Host, "https://")

	transport, upgrader, err := spdy.RoundTripperFor(kube.Config)
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &url.URL{Scheme: "https", Path: path, Host: hostIP})
	forwarder, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", localPort, podPort)}, stopChan, nil, nil, kube.Outputter)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	return forwarder.ForwardPorts()
}
