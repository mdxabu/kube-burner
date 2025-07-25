// Copyright 2020 The Kube-burner Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloud-bulldozer/go-commons/v2/indexers"
	uid "github.com/google/uuid"
	"github.com/kube-burner/kube-burner/pkg/alerting"
	"github.com/kube-burner/kube-burner/pkg/burner"
	"github.com/kube-burner/kube-burner/pkg/config"
	"github.com/kube-burner/kube-burner/pkg/measurements"
	"github.com/kube-burner/kube-burner/pkg/prometheus"
	"github.com/kube-burner/kube-burner/pkg/util"
	"github.com/kube-burner/kube-burner/pkg/util/fileutils"
	"github.com/kube-burner/kube-burner/pkg/util/metrics"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

var binName = filepath.Base(os.Args[0])

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   binName,
	Short: "Burn a kubernetes cluster",
	Long: `Kube-burner 🔥

Tool aimed at stressing a kubernetes cluster by creating or deleting lots of objects.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		util.ConfigureLogging(cmd)
	},
}

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate completion script for your shell",
	Long: `To load completions:

Bash:
  $ source <(kube-burner completion bash)

  # To load completions for each session, execute once:
  # Linux:
  $ kube-burner completion bash > /etc/bash_completion.d/kube-burner
  # macOS:
  $ kube-burner completion bash > /usr/local/etc/bash_completion.d/kube-burner

Zsh:
  # If shell completion is not already enabled in your environment,
  # you will need to enable it.  You can execute the following once:

  $ echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  $ kube-burner completion zsh > "${fpath[1]}/_kube-burner"

  # You will need to start a new shell for this setup to take effect.

Fish:
  $ kube-burner completion fish | source

  # To load completions for each session, execute once:
  $ kube-burner completion fish > ~/.config/fish/completions/kube-burner.fish

PowerShell:
  PS> kube-burner completion powershell | Out-String | Invoke-Expression

  # To load completions for every new session, run:
  PS> kube-burner completion powershell > kube-burner.ps1
  # and source this file from your PowerShell profile.
`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		}
		return fmt.Errorf("unsupported shell type %q", args[0])
	},
}

func initCmd() *cobra.Command {
	var err error
	var clientSet kubernetes.Interface
	var kubeConfig, kubeContext string
	var metricsEndpoint, configFile, configMap, metricsProfile, alertProfile string
	var uuid, userMetadata, namespace string
	var skipTLSVerify bool
	var timeout time.Duration
	var userDataFile string
	var allowMissingKeys bool
	var rc int
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Launch benchmark",
		PostRun: func(cmd *cobra.Command, args []string) {
			log.Info("👋 Exiting kube-burner ", uuid)
			os.Exit(rc)
		},
		Args: cobra.NoArgs,
		PreRun: func(cmd *cobra.Command, args []string) {
			if uuid == "" {
				uuid = uid.NewString()
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			if configMap != "" {
				metricsProfile, alertProfile, err = config.FetchConfigMap(configMap, namespace)
				if err != nil {
					log.Fatal(err.Error())
				}
				// We assume configFile is config.yml
				configFile = "config.yml"
			}
			util.SetupFileLogging(uuid)
			kubeClientProvider := config.NewKubeClientProvider(kubeConfig, kubeContext)
			clientSet, _ = kubeClientProvider.DefaultClientSet()
			configFileReader, err := fileutils.GetWorkloadReader(configFile, nil)
			if err != nil {
				log.Fatalf("Error reading configuration file %s: %s\nPlease ensure the file exists and is accessible", configFile, err)
			}
			var userDataFileReader io.Reader
			if userDataFile != "" {
				userDataFileReader, err = fileutils.GetWorkloadReader(userDataFile, nil)
				if err != nil {
					log.Fatalf("Error reading user data file %s: %s\nPlease ensure the file exists and is accessible", userDataFile, err)
				}
			}
			configSpec, err := config.ParseWithUserdata(uuid, timeout, configFileReader, userDataFileReader, allowMissingKeys, nil)
			if err != nil {
				log.Fatalf("Config error: %s", err.Error())
			}
			metricsScraper := metrics.ProcessMetricsScraperConfig(metrics.ScraperConfig{
				ConfigSpec:      &configSpec,
				MetricsEndpoint: metricsEndpoint,
				UserMetaData:    userMetadata,
				AlertProfile:    alertProfile,
				MetricsProfile:  metricsProfile,
			})
			if configSpec.GlobalConfig.ClusterHealth {
				clientSet, _ = kubeClientProvider.ClientSet(0, 0)
				util.ClusterHealthCheck(clientSet)
			}

			rc, err = burner.Run(configSpec, kubeClientProvider, metricsScraper, nil, nil)
			if err != nil {
				log.Error(err.Error())
				os.Exit(rc)
			}
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "Benchmark UUID (generated automatically if not provided)")
	cmd.Flags().StringVarP(&metricsEndpoint, "metrics-endpoint", "e", "", "YAML file with a list of metric endpoints")
	cmd.Flags().BoolVar(&skipTLSVerify, "skip-tls-verify", true, "Verify prometheus TLS certificate")
	cmd.Flags().DurationVarP(&timeout, "timeout", "", 4*time.Hour, "Benchmark timeout")
	cmd.Flags().StringVarP(&configFile, "config", "c", "", "Config file path or URL")
	cmd.Flags().StringVarP(&configMap, "configmap", "", "", "Configmap holding all the configuration: config.yml, metrics.yml and alerts.yml. metrics and alerts are optional")
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Namespace where the configmap is")
	cmd.Flags().StringVar(&userMetadata, "user-metadata", "", "User provided metadata file, in YAML format")
	cmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "Path to the kubeconfig file")
	cmd.Flags().StringVar(&kubeContext, "kube-context", "", "The name of the kubeconfig context to use")
	cmd.Flags().StringVar(&userDataFile, "user-data", "", "User provided data file for rendering the configuration file, in JSON or YAML format")
	cmd.Flags().BoolVar(&allowMissingKeys, "allow-missing", false, "Do not fail on missing values in the config file")
	cmd.Flags().SortFlags = false
	cmd.MarkFlagsMutuallyExclusive("config", "configmap")
	return cmd
}

func healthCheck() *cobra.Command {
	var kubeConfig, kubeContext string
	var rc int
	cmd := &cobra.Command{
		Use:   "health-check",
		Short: "Check for Health Status of the cluster",
		PostRun: func(cmd *cobra.Command, args []string) {
			log.Info("👋 Exiting kube-burner ")
			os.Exit(rc)
		},
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			var uuid = uid.NewString()
			util.SetupFileLogging(uuid)
			clientSet, _ := config.NewKubeClientProvider(kubeConfig, kubeContext).ClientSet(0, 0)
			util.ClusterHealthCheck(clientSet)
		},
	}
	cmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "Path to the kubeconfig file")
	cmd.Flags().StringVar(&kubeContext, "kube-context", "", "The name of the kubeconfig context to use")
	return cmd
}

func destroyCmd() *cobra.Command {
	var uuid string
	var timeout time.Duration
	var kubeConfig, kubeContext string
	var rc int
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy old namespaces labeled with the given UUID.",
		PostRun: func(cmd *cobra.Command, args []string) {
			log.Info("👋 Exiting kube-burner ", uuid)
			os.Exit(rc)
		},
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			util.SetupFileLogging(uuid)
			kubeClientProvider := config.NewKubeClientProvider(kubeConfig, kubeContext)
			clientSet, restConfig := kubeClientProvider.ClientSet(0, 0)
			dynamicClient := dynamic.NewForConfigOrDie(restConfig)
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			labelSelector := fmt.Sprintf("kube-burner-uuid=%s", uuid)
			util.CleanupNamespaces(ctx, clientSet, labelSelector)
			util.CleanupNonNamespacedResources(ctx, clientSet, dynamicClient, labelSelector)
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "UUID")
	cmd.Flags().DurationVarP(&timeout, "timeout", "", 4*time.Hour, "Deletion timeout")
	cmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "Path to the kubeconfig file")
	cmd.Flags().StringVar(&kubeContext, "kube-context", "", "The name of the kubeconfig context to use")
	cmd.MarkFlagRequired("uuid")
	return cmd
}

func measureCmd() *cobra.Command {
	var uuid string
	var rawNamespaces string
	var selector string
	var configFile string
	var jobName string
	var userMetadata string
	var kubeConfig, kubeContext string
	indexerList := make(map[string]indexers.Indexer)
	metadata := make(map[string]any)
	cmd := &cobra.Command{
		Use:   "measure",
		Short: "Take measurements for a given set of resources without running workload",
		PostRun: func(cmd *cobra.Command, args []string) {
			log.Info("👋 Exiting kube-burner ", uuid)
		},
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			util.SetupFileLogging(uuid)
			f, err := fileutils.GetWorkloadReader(configFile, nil)
			if err != nil {
				log.Fatalf("Error reading configuration file %s: %s", configFile, err)
			}
			configSpec, err := config.Parse(configFile, time.Hour, f)
			if err != nil {
				log.Fatal(err.Error())
			}
			if len(configSpec.Jobs) > 0 {
				log.Fatal("No jobs are allowed in a measure subcommand config file")
			}
			for pos, indexer := range configSpec.MetricsEndpoints {
				log.Infof("📁 Creating indexer: %s", indexer.Type)
				idx, err := indexers.NewIndexer(indexer.IndexerConfig)
				if err != nil {
					log.Fatalf("Error creating indexer %d: %v", pos, err.Error())
				}
				indexerList[indexer.Alias] = *idx
			}
			if userMetadata != "" {
				metadata, err = util.ReadUserMetadata(userMetadata)
				if err != nil {
					log.Fatalf("Error reading provided user metadata: %v", err)
				}
			}
			labelSelector, err := labels.Parse(selector)
			if err != nil {
				log.Fatalf("Invalid selector: %v", err)
			}
			namespaceLabels := make(map[string]string)
			namespaceAnnotations := make(map[string]string)
			labelRequirements, _ := labelSelector.Requirements()
			for _, req := range labelRequirements {
				namespaceLabels[req.Key()] = req.Values().List()[0]
			}
			log.Infof("%v", namespaceLabels)
			measurementsInstance := measurements.NewMeasurementsFactory(configSpec, metadata, nil).NewMeasurements(
				&config.Job{
					Name:                 jobName,
					Namespace:            rawNamespaces,
					NamespaceLabels:      namespaceLabels,
					NamespaceAnnotations: namespaceAnnotations,
				},
				config.NewKubeClientProvider(kubeConfig, kubeContext),
				nil,
			)
			measurementsInstance.Collect()
			if err = measurementsInstance.Stop(); err != nil {
				log.Error(err.Error())
			}
			measurementsInstance.Index(jobName, indexerList)
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "UUID")
	cmd.Flags().StringVar(&userMetadata, "user-metadata", "", "User provided metadata file, in YAML format")
	cmd.Flags().StringVarP(&configFile, "config", "c", "config.yml", "Config file path or URL")
	cmd.Flags().StringVarP(&jobName, "job-name", "j", "kube-burner-measure", "Measure job name")
	cmd.Flags().StringVarP(&rawNamespaces, "namespaces", "n", corev1.NamespaceAll, "comma-separated list of namespaces")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "namespace label selector. (e.g. -l key1=value1,key2=value2)")
	cmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "Path to the kubeconfig file")
	cmd.Flags().StringVar(&kubeContext, "kube-context", "", "The name of the kubeconfig context to use")
	return cmd
}

func indexCmd() *cobra.Command {
	var url, metricsEndpoint, metricsProfile, jobName string
	var start, end int64
	var username, password, uuid, token, userMetadata string
	var esServer, esIndex, metricsDirectory string
	var configSpec config.Spec
	var skipTLSVerify bool
	var prometheusStep time.Duration
	var tarballName string
	var indexer config.MetricsEndpoint
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Index kube-burner metrics",
		Long:  "If no other indexer is specified, local indexer is used by default",
		Args:  cobra.NoArgs,
		PostRun: func(cmd *cobra.Command, args []string) {
			log.Info("👋 Exiting kube-burner ", uuid)
		},
		PreRun: func(cmd *cobra.Command, args []string) {
			if uuid == "" {
				uuid = uid.NewString()
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			util.SetupFileLogging(uuid)
			configSpec.GlobalConfig.UUID = uuid
			metricsProfiles := strings.FieldsFunc(metricsProfile, func(r rune) bool {
				return r == ',' || r == ' '
			})
			indexer = config.MetricsEndpoint{
				Username:      username,
				Password:      password,
				Token:         token,
				Step:          prometheusStep,
				Endpoint:      url,
				Metrics:       metricsProfiles,
				SkipTLSVerify: skipTLSVerify,
			}
			if esServer != "" && esIndex != "" {
				indexer.IndexerConfig = indexers.IndexerConfig{
					Type:    indexers.ElasticIndexer,
					Servers: []string{esServer},
					Index:   esIndex,
				}
			} else {
				indexer.IndexerConfig = indexers.IndexerConfig{
					Type:             indexers.LocalIndexer,
					MetricsDirectory: metricsDirectory,
					TarballName:      tarballName,
				}
			}
			configSpec.MetricsEndpoints = append(configSpec.MetricsEndpoints, indexer)
			metricsScraper := metrics.ProcessMetricsScraperConfig(metrics.ScraperConfig{
				ConfigSpec:      &configSpec,
				MetricsEndpoint: metricsEndpoint,
				UserMetaData:    userMetadata,
			})
			for _, prometheusClient := range metricsScraper.PrometheusClients {
				prometheusJob := prometheus.Job{
					Start: time.Unix(start, 0),
					End:   time.Unix(end, 0),
					JobConfig: config.Job{
						Name: jobName,
					},
				}
				if err := prometheusClient.ScrapeJobsMetrics(prometheusJob); err != nil {
					log.Fatal(err)
				}
			}
			if configSpec.MetricsEndpoints[0].Type == indexers.LocalIndexer && tarballName != "" {
				if err := metrics.CreateTarball(configSpec.MetricsEndpoints[0].IndexerConfig); err != nil {
					log.Fatal(err)
				}
			}
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "Benchmark UUID (generated automatically if not provided)")
	cmd.Flags().StringVarP(&url, "prometheus-url", "u", "", "Prometheus URL")
	cmd.Flags().StringVarP(&token, "token", "t", "", "Prometheus Bearer token")
	cmd.Flags().StringVar(&username, "username", "", "Prometheus username for authentication")
	cmd.Flags().StringVarP(&password, "password", "p", "", "Prometheus password for basic authentication")
	cmd.Flags().StringVarP(&metricsProfile, "metrics-profile", "m", "metrics.yml", "comma-separated list of metric profiles")
	cmd.Flags().StringVarP(&metricsEndpoint, "metrics-endpoint", "e", "", "YAML file with a list of metric endpoints")
	cmd.Flags().BoolVar(&skipTLSVerify, "skip-tls-verify", true, "Verify prometheus TLS certificate")
	cmd.Flags().DurationVarP(&prometheusStep, "step", "s", 30*time.Second, "Prometheus step size")
	cmd.Flags().Int64VarP(&start, "start", "", time.Now().Unix()-3600, "Epoch start time")
	cmd.Flags().Int64VarP(&end, "end", "", time.Now().Unix(), "Epoch end time")
	cmd.Flags().StringVarP(&jobName, "job-name", "j", "kube-burner-indexing", "Indexing job name")
	cmd.Flags().StringVar(&userMetadata, "user-metadata", "", "User provided metadata file, in YAML format")
	cmd.Flags().StringVar(&metricsDirectory, "metrics-directory", "collected-metrics", "Directory to dump the metrics files in, when using default local indexing")
	cmd.Flags().StringVar(&esServer, "es-server", "", "Elastic Search endpoint")
	cmd.Flags().StringVar(&esIndex, "es-index", "", "Elastic Search index")
	cmd.Flags().StringVar(&tarballName, "tarball-name", "", "Dump collected metrics into a tarball with the given name, requires local indexing")
	cmd.Flags().SortFlags = false
	return cmd
}

func importCmd() *cobra.Command {
	var tarball string
	var esServer, esIndex, metricsDirectory string
	var indexerConfig indexers.IndexerConfig
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import metrics tarball",
		Run: func(cmd *cobra.Command, args []string) {
			if esServer != "" && esIndex != "" {
				indexerConfig = indexers.IndexerConfig{
					Type:    indexers.ElasticIndexer,
					Servers: []string{esServer},
					Index:   esIndex,
				}
			} else {
				indexerConfig = indexers.IndexerConfig{
					Type:             indexers.LocalIndexer,
					MetricsDirectory: metricsDirectory,
				}
			}
			log.Infof("📁 Creating indexer: %s", indexerConfig.Type)
			indexer, err := indexers.NewIndexer(indexerConfig)
			if err != nil {
				log.Fatal(err.Error())
			}
			err = metrics.ImportTarball(tarball, indexer)
			if err != nil {
				log.Fatal(err.Error())
			}
		},
	}
	cmd.Flags().StringVar(&tarball, "tarball", "", "Metrics tarball file")
	cmd.Flags().StringVar(&metricsDirectory, "metrics-directory", "collected-metrics", "Directory to dump the metrics files in, when using default local indexing")
	cmd.Flags().StringVar(&esServer, "es-server", "", "Elastic Search endpoint")
	cmd.Flags().StringVar(&esIndex, "es-index", "", "Elastic Search index")
	cmd.MarkFlagRequired("tarball")
	return cmd
}

func alertCmd() *cobra.Command {
	var configSpec config.Spec
	var err error
	var url, alertProfile, username, password, uuid, token string
	var esServer, esIndex, metricsDirectory string
	var start, end int64
	var skipTLSVerify bool
	var alertM *alerting.AlertManager
	var prometheusStep time.Duration
	var indexer *indexers.Indexer
	var indexerConfig indexers.IndexerConfig
	cmd := &cobra.Command{
		Use:   "check-alerts",
		Short: "Evaluate alerts for the given time range",
		Args:  cobra.NoArgs,
		PreRun: func(cmd *cobra.Command, args []string) {
			if uuid == "" {
				uuid = uid.NewString()
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			configSpec.GlobalConfig.UUID = uuid
			if esServer != "" && esIndex != "" {
				indexerConfig = indexers.IndexerConfig{
					Type:    indexers.ElasticIndexer,
					Servers: []string{esServer},
					Index:   esIndex,
				}
			} else if metricsDirectory != "" {
				indexerConfig = indexers.IndexerConfig{
					Type:             indexers.LocalIndexer,
					MetricsDirectory: metricsDirectory,
				}
			}
			if indexerConfig.Type != "" {
				log.Infof("📁 Creating indexer: %s", indexerConfig.Type)
				indexer, err = indexers.NewIndexer(indexerConfig)
				if err != nil {
					log.Fatal(err.Error())
				}
			}
			auth := prometheus.Auth{
				Username:      username,
				Password:      password,
				Token:         token,
				SkipTLSVerify: skipTLSVerify,
			}
			p, err := prometheus.NewPrometheusClient(configSpec, url, auth, prometheusStep, nil, indexer)
			if err != nil {
				log.Fatal(err)
			}
			job := prometheus.Job{
				Start: time.Unix(start, 0),
				End:   time.Unix(end, 0),
			}
			if alertM, err = alerting.NewAlertManager(alertProfile, uuid, p, indexer, nil, nil); err != nil {
				log.Fatalf("Error creating alert manager: %s", err)
			}
			err = alertM.Evaluate(job)
			log.Info("👋 Exiting kube-burner ", uuid)
			if err != nil {
				os.Exit(1)
			}
		},
	}
	cmd.Flags().StringVar(&uuid, "uuid", "", "Benchmark UUID (generated automatically if not provided)")
	cmd.Flags().StringVarP(&url, "prometheus-url", "u", "", "Prometheus URL")
	cmd.Flags().StringVarP(&token, "token", "t", "", "Prometheus Bearer token")
	cmd.Flags().StringVar(&username, "username", "", "Prometheus username for authentication")
	cmd.Flags().StringVarP(&password, "password", "p", "", "Prometheus password for basic authentication")
	cmd.Flags().StringVarP(&alertProfile, "alert-profile", "a", "alerts.yaml", "Alert profile file or URL")
	cmd.Flags().BoolVar(&skipTLSVerify, "skip-tls-verify", true, "Verify prometheus TLS certificate")
	cmd.Flags().DurationVarP(&prometheusStep, "step", "s", 30*time.Second, "Prometheus step size")
	cmd.Flags().Int64VarP(&start, "start", "", time.Now().Unix()-3600, "Epoch start time")
	cmd.Flags().Int64VarP(&end, "end", "", time.Now().Unix(), "Epoch end time")
	cmd.Flags().StringVar(&metricsDirectory, "metrics-directory", "", "Directory to dump the alert files in, enables local indexing when specified")
	cmd.Flags().StringVar(&esServer, "es-server", "", "Elastic Search endpoint")
	cmd.Flags().StringVar(&esIndex, "es-index", "", "Elastic Search index")
	cmd.MarkFlagRequired("prometheus-url")
	cmd.MarkFlagRequired("alert-profile")
	cmd.Flags().SortFlags = false
	return cmd
}

// executes rootCmd
func main() {
	util.SetupCmd(rootCmd)
	rootCmd.AddCommand(
		initCmd(),
		measureCmd(),
		destroyCmd(),
		healthCheck(),
		indexCmd(),
		alertCmd(),
		importCmd(),
		completionCmd,
	)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
