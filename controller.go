package main

import (
	"context"
	"fmt"
	"time"

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
)

type Controller struct {
	kubeclientset    kubernetes.Interface
	gatewayclientset clientset.Interface

	gatewayClassLister listers.GatewayClassLister
	gatewayClassSynced cache.InformerSynced

	workqueue workqueue.RateLimitingInterface
	recorder  record.EventRecorder
}

func NewController(
	ctx context.Context,
	kubeclientset kubernetes.Interface,
	gatewayclientset clientset.Interface,
	gatewayClassInformer informers.GatewayClassInformer) *Controller {

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
		kubeclientset:      kubeclientset,
		gatewayclientset:   gatewayclientset,
		gatewayClassLister: listers.NewGatewayClassLister(gatewayClassInformer.Informer().GetIndexer()),
		gatewayClassSynced: gatewayClassInformer.Informer().HasSynced,
		workqueue:          workqueue.NewRateLimitingQueue(ratelimiter),
		recorder:           recorder,
	}

	logger.Info("Setting up event handlers")
	gatewayClassInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueGatewayClass,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueGatewayClass(new)
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
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(ctx, key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		logger.Info("Successfully synced", "resourceName", key)
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
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	// logger := klog.LoggerWithValues(klog.FromContext(ctx), "resourceName", key)

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the GatewayClass resource with this namespace/name
	gc, err := c.gatewayClassLister.Get(name)
	if err != nil {
		// The GatewayClass resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("gatewayclass '%s' in work queue no longer exists", key))
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
	return nil
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

func (c *Controller) enqueueGatewayClass(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}
