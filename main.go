package main

import (
	"flag"
	"os"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	klog "k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	clientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	informers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

var (
	masterURL  string
	kubeconfig string
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	ctx := signals.SetupSignalHandler()
	logger := klog.FromContext(ctx)

	logger.Info("Initializing cloudflare client")
	cloudflareApi, err := cloudflare.NewWithAPIToken(os.Getenv("CLOUDFLARE_TOKEN"))
	if err != nil {
		logger.Error(err, "Error initializing cloudflare client")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	logger.Info("Initializing kube client")
	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		logger.Error(err, "Error building kubeconfig")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "Error building kubernetes clientset")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	clientset, err := clientset.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "Error building kubernetes clientset")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	informerFactory := informers.NewSharedInformerFactory(clientset, time.Second*30)

	controller := NewController(
		ctx,
		cloudflareApi,
		kubeClient, clientset,
		informerFactory.Gateway().V1().GatewayClasses(),
		informerFactory.Gateway().V1().Gateways(),
	)
	informerFactory.Start(ctx.Done())

	if err = controller.Run(ctx, 2); err != nil {
		logger.Error(err, "Error running controller")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
}
