package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ViBiOh/kmux/pkg/client"
	"github.com/ViBiOh/kmux/pkg/output"
	"github.com/ViBiOh/kmux/pkg/resource"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/homedir"
)

var (
	clients      client.Array
	allNamespace bool
)

var rootCmd = &cobra.Command{
	Use:   "kmux",
	Short: "Multiplexing kubectl common tasks accross clusters",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		var err error
		clients, err = getKubernetesClient(viper.GetStringSlice("context"))
		if err != nil {
			output.Fatal("%s", err)
		}
	},
	PersistentPostRun: func(_ *cobra.Command, _ []string) {
		output.Close()
		<-output.Done()
	},
	Run: func(cmd *cobra.Command, args []string) {
		clients.Execute(func(kube client.Kube) error {
			info, err := kube.Discovery().ServerVersion()
			if err != nil {
				return fmt.Errorf("get server version: %w", err)
			}

			kube.Std("Cluster version: %s\nNamespace: %s", info, kube.Namespace)

			return nil
		})
	},
}

func getKubernetesClient(contexts []string) (client.Array, error) {
	var clientsArray client.Array

	configRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: viper.GetString("kubeconfig")}

	if len(contexts) == 0 {
		contexts = append(contexts, "")
	}

	for _, context := range contexts {
		kubeClient, err := getKubeClient(configRules, context)
		if err != nil {
			return clientsArray, err
		}

		clientsArray = append(clientsArray, kubeClient)
	}

	return clientsArray, nil
}

func getKubeClient(configRules *clientcmd.ClientConfigLoadingRules, context string) (client.Kube, error) {
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: context,
		Context: api.Context{
			Namespace: viper.GetString("namespace"),
		},
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(configRules, configOverrides)
	k8sConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return client.Kube{}, fmt.Errorf("read kubernetes config file: %w", err)
	}

	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		return client.Kube{}, fmt.Errorf("read configured namespace: %w", err)
	}

	if allNamespace {
		namespace = ""
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return client.Kube{}, fmt.Errorf("create kubernetes client: %w", err)
	}

	return client.New(context, namespace, clientset), nil
}

func init() {
	viper.AutomaticEnv()

	flags := rootCmd.PersistentFlags()

	var defaultConfig string
	if home := homedir.HomeDir(); home != "" {
		defaultConfig = filepath.Join(home, ".kube", "config")
	}

	flags.String("kubeconfig", defaultConfig, "Kubernetes configuration file")
	if err := viper.BindPFlag("kubeconfig", flags.Lookup("kubeconfig")); err != nil {
		output.Fatal("bind `kubeconfig` flag: %s", err)
	}

	flags.StringSlice("context", nil, "Kubernetes context, multiple for mutiplexing commands")
	if err := viper.BindPFlag("context", flags.Lookup("context")); err != nil {
		output.Fatal("bind `context` flag: %s", err)
	}

	if err := rootCmd.RegisterFlagCompletionFunc("context", completeContext); err != nil {
		output.Fatal("register `context` flag completion: %s", err)
	}

	flags.BoolVarP(&allNamespace, "all-namespaces", "A", false, "Find resources in all namespaces")

	flags.StringP("namespace", "n", "", "Override kubernetes namespace in context")
	if err := viper.BindPFlag("namespace", flags.Lookup("namespace")); err != nil {
		output.Fatal("bind `namespace` flag: %s", err)
	}

	if err := rootCmd.RegisterFlagCompletionFunc("namespace", completeNamespace); err != nil {
		output.Fatal("register `namespace` flag completion: %s", err)
	}

	rootCmd.AddCommand(imageCmd)
	rootCmd.AddCommand(restartCmd)

	initWatch()
	rootCmd.AddCommand(watchCmd)

	initLog()
	rootCmd.AddCommand(logCmd)
}

func completeContext(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	configRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: viper.GetString("kubeconfig")}
	config, err := configRules.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	contexts := viper.GetStringSlice("context")

	var completeContexts []string
	for name := range config.Contexts {
		if contains(contexts, name) {
			continue
		}
		completeContexts = append(completeContexts, name)
	}

	return completeContexts, cobra.ShellCompDirectiveNoFileComp
}

func completeNamespace(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	lister, err := resource.ListerFor("namespace")
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	clients, err = getKubernetesClient(strings.Split(viper.GetString("context"), ","))
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	return getCommonObjects("", lister), cobra.ShellCompDirectiveDefault
}

func contains(arr []string, value string) bool {
	for _, item := range arr {
		if strings.EqualFold(item, value) {
			return true
		}
	}

	return false
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
