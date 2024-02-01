package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/scheme"

	v1 "sigs.k8s.io/gateway-api/apis/v1"
	clientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	informers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions/apis/v1"
	listers "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

const controllerAgentName = "pokgak.xyz/cloudflare-gateway-api-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Foo is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Foo fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Foo"
	// MessageResourceSynced is the message used for an Event fired when a Foo
	// is synced successfully
	MessageResourceSynced = "Resource synced successfully"

	GatewayClass = "GatewayClass"
	Gateway      = "Gateway"
	HTTPRoute    = "HTTPRoute"
)

var (
	CloudflareAccountIdentifier = cloudflare.AccountIdentifier("d572f4f92aebab4f66ceb1004be4d164")
)

type WorkqueueItem struct {
	Kind string
	Key  string
}

type Controller struct {
	cloudflareApi    *cloudflare.API
	kubeclientset    kubernetes.Interface
	gatewayclientset clientset.Interface

	gatewayClassLister listers.GatewayClassLister
	gatewayClassSynced cache.InformerSynced
	gatewayLister      listers.GatewayLister
	gatewaySynced      cache.InformerSynced

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

func NewController(
	ctx context.Context,
	cloudflareApi *cloudflare.API,
	kubeclientset kubernetes.Interface,
	gatewayclientset clientset.Interface,
	gatewayClassInformer informers.GatewayClassInformer,
	gatewayInformer informers.GatewayInformer) *Controller {

	utilruntime.Must(v1.AddToScheme(scheme.Scheme))
	logger := klog.FromContext(ctx)

	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	ratelimiter := workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	controller := &Controller{
		cloudflareApi:      cloudflareApi,
		kubeclientset:      kubeclientset,
		gatewayclientset:   gatewayclientset,
		gatewayClassLister: listers.NewGatewayClassLister(gatewayClassInformer.Informer().GetIndexer()),
		gatewayClassSynced: gatewayClassInformer.Informer().HasSynced,
		gatewayLister:      listers.NewGatewayLister(gatewayInformer.Informer().GetIndexer()),
		gatewaySynced:      gatewayInformer.Informer().HasSynced,
		workqueue:          workqueue.NewRateLimitingQueue(ratelimiter),
		recorder:           recorder,
	}

	logger.Info("Setting up event handlers")
	gatewayClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.enqueueEvent(obj, GatewayClass)
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueEvent(new, GatewayClass)
		},
	})

	gatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.enqueueEvent(obj, Gateway)
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueEvent(new, Gateway)
		},
	})

	return controller
}

func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)

	logger.Info("Starting Gateway API controller")
	logger.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.gatewayClassSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count", workers)
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logger.Info("Started workers")
	<-ctx.Done()
	logger.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var item WorkqueueItem
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if item, ok = obj.(WorkqueueItem); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected WorkqueuItem in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// resource to be synced.
		if err := c.syncHandler(ctx, item); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(item)
			return fmt.Errorf("error syncing '%s': %s, requeuing", item.Key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		logger.Info("Successfully synced", "resourceName", item.Key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Foo resource
// with the current status of the resource.
func (c *Controller) syncHandler(ctx context.Context, item WorkqueueItem) error {
	logger := klog.FromContext(ctx)

	// Convert the namespace/name string into a distinct namespace and name
	// logger := klog.LoggerWithValues(klog.FromContext(ctx), "resourceName", key)

	ns, name, err := cache.SplitMetaNamespaceKey(item.Key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", item.Key))
		return nil
	}

	if ns == "" {
		// Get the GatewayClass resource with this name
		gc, err := c.gatewayClassLister.Get(name)
		if err != nil {
			// The GatewayClass resource may no longer exist, in which case we stop
			// processing.
			if errors.IsNotFound(err) {
				utilruntime.HandleError(fmt.Errorf("resource '%s' '%s' in work queue no longer exists", item.Kind, item.Key))
				return nil
			}

			return err
		}

		// skip update if no changes to the resource
		if gc.Status.Conditions[len(gc.Status.Conditions)-1].ObservedGeneration == gc.Generation {
			return nil
		}

		// Finally, we update the status block of the Foo resource to reflect the
		// current state of the world
		err = c.updateGatewayClassStatus(gc)
		if err != nil {
			return err
		}

		c.recorder.Event(gc, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	} else {
		logger.Info("received Gateway event", "namespace", ns, "name", name)

		switch item.Kind {
		case Gateway:
			g, err := c.gatewayLister.Gateways(ns).Get(name)
			if err != nil {
				// The Gateway resource may no longer exist, in which case we stop
				// processing.
				if errors.IsNotFound(err) {
					utilruntime.HandleError(fmt.Errorf("resource '%s' '%s' in work queue no longer exists", item.Kind, item.Key))
					return nil
				}

				return err
			}

			// skip update if no changes to the resource
			if g.Status.Conditions[len(g.Status.Conditions)-1].ObservedGeneration == g.Generation {
				return nil
			}

			if tunnelId, ok := g.GetAnnotations()["cloudflare.com/tunnel-id"]; ok {
				logger.Info("Gateway resource", "namespace", g.Namespace, "name", g.Name, "generation", g.Generation, "tunnelId", tunnelId)

				logger.Info("Tunnel exists, checking status")
				tunnel, err := c.cloudflareApi.GetTunnel(ctx, CloudflareAccountIdentifier, tunnelId)
				if err != nil {
					logger.Info("Failed to get tunnel", "id", tunnelId, "error", err)
					return err
				}
				logger.Info("Tunnel", "id", tunnel.ID, "name", tunnel.Name, "status", tunnel.Status)

				logger.Info("Gateway status", g.Status.Conditions[len(g.Status.Conditions)-1].String())
			} else {
				logger.Info("Tunnel does not exist, creating")

				// generate a random 32-byte, base64-encoded random string
				tunnelSecret, err := generateTunnelSecret()
				if err != nil {
					logger.Error(err, "Failed to generate tunnel secret")
					return err
				}

				params := cloudflare.TunnelCreateParams{
					Name:      g.Name,
					Secret:    tunnelSecret,
					ConfigSrc: "cloudflare",
				}

				tunnel, err := c.cloudflareApi.CreateTunnel(ctx, CloudflareAccountIdentifier, params)
				if err != nil {
					logger.Error(err, "Failed to create tunnel")
					return err
				}

				logger.Info("Tunnel created", "id", tunnel.ID, "name", tunnel.Name, "status", tunnel.Status)
				logger.Info("Updating Gateway resource with tunnel id", tunnel.ID)

				gCopy := g.DeepCopy()
				gCopy.Annotations["cloudflare.com/tunnel-id"] = tunnel.ID
				gCopy.Annotations["cloudflare.com/tunnel-secret"] = tunnelSecret

				conditionAccepted := metav1.Condition{
					Status:             metav1.ConditionTrue,
					Type:               string(v1.GatewayConditionAccepted),
					Reason:             string(v1.GatewayReasonAccepted),
					ObservedGeneration: gCopy.Generation,
					LastTransitionTime: metav1.Now(),
				}

				conditionProgrammed := metav1.Condition{
					Status:             metav1.ConditionTrue,
					Type:               string(v1.GatewayConditionProgrammed),
					Reason:             string(v1.GatewayReasonAccepted),
					ObservedGeneration: gCopy.Generation,
					LastTransitionTime: metav1.Now(),
				}
				gCopy.Status.Conditions = append(gCopy.Status.Conditions, conditionAccepted, conditionProgrammed)

				logger.Info("Gateway status", gCopy.Status.Conditions[len(gCopy.Status.Conditions)-1].String())

				_, err = c.gatewayclientset.GatewayV1().Gateways(gCopy.Namespace).Update(context.Background(), gCopy, metav1.UpdateOptions{})
				if err != nil {
					return err
				}
			}
		}

	}
	return nil
}

func generateTunnelSecret() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(b), nil
}

func (c *Controller) updateGatewayClassStatus(gc *v1.GatewayClass) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	gcCopy := gc.DeepCopy()
	condition := metav1.Condition{
		Status:             metav1.ConditionTrue,
		Type:               string(v1.GatewayClassConditionStatusAccepted),
		Reason:             string(v1.GatewayClassReasonAccepted),
		Message:            "GatewayClass accepted by controller",
		ObservedGeneration: gc.Generation,
		LastTransitionTime: metav1.Now(),
	}

	// check last condition type already "Accepted"
	if gcCopy.Status.Conditions[len(gcCopy.Status.Conditions)-1].Type == string(v1.GatewayClassConditionStatusAccepted) {
		gcCopy.Status.Conditions[len(gcCopy.Status.Conditions)-1] = condition
		_, err := c.gatewayclientset.GatewayV1().GatewayClasses().UpdateStatus(context.Background(), gcCopy, metav1.UpdateOptions{})
		return err
	} else {
		gcCopy.Status.Conditions = append(gcCopy.Status.Conditions, condition)
		_, err := c.gatewayclientset.GatewayV1().GatewayClasses().UpdateStatus(context.Background(), gcCopy, metav1.UpdateOptions{})
		return err
	}
}

func (c *Controller) enqueueEvent(obj interface{}, kind string) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}

	item := WorkqueueItem{
		Kind: kind,
		Key:  key,
	}

	c.workqueue.Add(item)
}
