/*
Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"bufio"
	"context"
	_ "embed"
	"flag"
	"fmt"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/awslabs/eks-auto-mode-ebs-migration-tool/pkg/migrator"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
)

//go:generate cp -r ../../ATTRIBUTION.md ./
//go:embed ATTRIBUTION.md
var attribution string

// set via linker flags
var (
	version = "dev"
	commit  = ""
	date    = ""
	builtBy = "manual"
)

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `The eks-auto-mode-ebs-migration-tool is used to migrate a Persistent Volume Claim from a 
standard EBS CSI StorageClass (ebs.csi.aws.com) to the EKS Auto EBS CSI StorageClass 
(ebs.csi.eks.amazonaws.com) or vice versa. To do this, it must delete the PVC/PV that
are currently in use and replace them with new copies updated to use the new StorageClass.
Workloads using the volume must be scaled down or terminated before use, as the EBS Volume
must be detached prior to migration.`)
	fmt.Fprintln(flag.CommandLine.Output())
	fmt.Fprintln(flag.CommandLine.Output())
	fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	klog.InitFlags(nil)
	kubeconfigDefault := filepath.Join(homedir.HomeDir(), ".kube", "config")
	if envKubeConfig := os.Getenv("KUBECONFIG"); envKubeConfig != "" {
		kubeconfigDefault = envKubeConfig
	}
	kubeConfig := flag.String("kubeconfig", kubeconfigDefault, "Absolute path to the kubeconfig file")
	namespace := flag.String("namespace", "default", "Namespace for the PVC")
	pvcName := flag.String("pvc-name", "", "Name of the PVC")
	newStorageClassName := flag.String("storageclass", "", "New storage class to migrate to")
	clusterName := flag.String("cluster-name", "", "Name of the cluster")
	snapshot := flag.Bool("snapshot", true, "Create a snapshot of the EBS volume prior to making any changes")
	dryRun := flag.Bool("dry-run", true, "Run in dry-run mode where validations are performed, but no mutations occur")
	showAttribution := flag.Bool("attribution", false, "Show attribution")
	yes := flag.Bool("yes", false, "Override the prompt to accept migration if all validations have passed")
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showAttribution {
		fmt.Println(attribution)
		os.Exit(0)
	}
	if *versionFlag {
		fmt.Printf("eks-auto-mode-ebs-migration-tool %s\n", version)
		fmt.Printf("commit: %s\n", commit)
		fmt.Printf("built at: %s\n", date)
		fmt.Printf("built by: %s\n", builtBy)
		os.Exit(0)
	}
	if *dryRun {
		klog.InfoS("Running in dry-run mode")
	} else {
		klog.InfoS("Running in mutate mode")
	}

	mcfg := migrator.Config{
		ClusterName:         *clusterName,
		Namespace:           *namespace,
		NewStorageClassName: *newStorageClassName,
		PVCName:             *pvcName,
	}
	if err := mcfg.Validate(); err != nil {
		klog.ErrorS(err, "Error validating input")
		fmt.Println()
		flag.Usage()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeConfig)
	if err != nil {
		klog.ErrorS(err, "Error building K8s client config")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.ErrorS(err, "Error creating PVC client")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	cfg, err := awscfg.LoadDefaultConfig(ctx)
	if err != nil {
		klog.ErrorS(err, "Unable to load AWS configuration")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		return
	}

	m := migrator.New(cs, cfg, mcfg)

	// first validate any preconditions, perform dry-run checks, etc.
	if err := m.ValidatePreconditions(ctx); err != nil {
		klog.ErrorS(err, "Precondition checks failed")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	// if we're in dry-run mode, stop there
	if *dryRun {
		klog.InfoS("Dry-run completed successfully")
		os.Exit(0)
	}

	if *snapshot {
		if err := m.PerformSnapshot(ctx); err != nil {
			klog.ErrorS(err, "Error creating snapshot")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}

	// check if the user is overriding the prompt
	if *yes {
		klog.InfoS("Skipping prompt due to `--yes`")
	} else {
		if userInput := promptUserInput("Validations were successful. The following operations can fail and will require manual intervention to repair in that case. Type YES to continue with migration"); userInput != "YES" {
			klog.InfoS("Aborting, no changes have been made")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}

	// Up until now, we've just described and sanity checked things.  We have to make a few changes, which if they fail
	// put us in a really bad state. We start by deleting the PVC.  Since the volume name is formed by the PVC UID, which
	// we can't control, manual recovery is the only way out from here.
	if err := m.Execute(ctx); err != nil {
		klog.ErrorS(err, "Error executing migration")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	klog.InfoS("Migration complete!")
}

func promptUserInput(prompt string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println(prompt)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	return input
}
