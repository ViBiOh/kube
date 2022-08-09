package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ViBiOh/kube/pkg/client"
	"github.com/ViBiOh/kube/pkg/output"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/homedir"
)

var clients client.Array

var rootCmd = &cobra.Command{
	Use:   "kube",
	Short: "Kube simplify use of kubectl",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		var err error
		clients, err = getKubernetesClient(strings.Split(viper.GetString("context"), ","))
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
	var output client.Array

	for _, context := range contexts {
		configOverrides := &clientcmd.ConfigOverrides{
			CurrentContext: context,
			Context: api.Context{
				Namespace: viper.GetString("namespace"),
			},
		}
		configRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: viper.GetString("kubeconfig")}

		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(configRules, configOverrides)
		k8sConfig, err := clientConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("read kubernetes config file: %w", err)
		}

		namespace, _, err := clientConfig.Namespace()
		if err != nil {
			return nil, fmt.Errorf("read configured namespace: %w", err)
		}

		clientset, err := kubernetes.NewForConfig(k8sConfig)
		if err != nil {
			return nil, fmt.Errorf("create kubernetes client: %w", err)
		}

		output = append(output, client.New(context, namespace, clientset))
	}

	return output, nil
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

	flags.String("context", "", "Kubernetes context, comma separated for mutiplexing commands")
	if err := viper.BindPFlag("context", flags.Lookup("context")); err != nil {
		output.Fatal("unable bind `context` flag: %s", err)
	}
	if err := viper.BindEnv("context", "KUBECONTEXT"); err != nil {
		output.Fatal("unable bind env `KUBECONTEXT`: %s", err)
	}

	flags.StringP("namespace", "n", "", "Override kubernetes namespace in context")
	if err := viper.BindPFlag("namespace", flags.Lookup("namespace")); err != nil {
		output.Fatal("unable bind `namespace` flag: %s", err)
	}

	rootCmd.AddCommand(imageCmd)

	initLog()
	rootCmd.AddCommand(logCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
