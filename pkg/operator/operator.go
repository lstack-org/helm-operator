package operator

import (
	"fmt"
	"github.com/lstack-org/helm-operator/pkg/chartsync"
	"os"
	"path"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/lstack-org/helm-operator/internal/lockedfile"
	helmfluxv1 "github.com/lstack-org/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	ifscheme "github.com/lstack-org/helm-operator/pkg/client/clientset/versioned/scheme"
	hrv1 "github.com/lstack-org/helm-operator/pkg/client/informers/externalversions/helm.fluxcd.io/v1"
	iflister "github.com/lstack-org/helm-operator/pkg/client/listers/helm.fluxcd.io/v1"
	"github.com/lstack-org/helm-operator/pkg/release"
	"github.com/lstack-org/helm-operator/pkg/status"
)

const (
	controllerAgentName = "helm-operator"
	ReleaseSynced       = "ReleaseSynced"
	FailedReleaseSync   = "FailedReleaseSync"
)

// Controller is the operator implementation for HelmRelease resources
type Controller struct {
	logger   log.Logger
	logDiffs bool

	hrLister iflister.HelmReleaseLister
	hrSynced cache.InformerSynced

	release      *release.Release
	gitChartSync *chartsync.GitChartSync

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	releaseWorkqueue workqueue.RateLimitingInterface

	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// New returns a new helm-operator
func New(
	logger log.Logger,
	logReleaseDiffs bool,
	kubeclientset kubernetes.Interface,
	hrInformer hrv1.HelmReleaseInformer,
	releaseWorkqueue workqueue.RateLimitingInterface,
	release *release.Release,
	gitChartSync *chartsync.GitChartSync) *Controller {

	// Add helm-operator types to the default Kubernetes Scheme so Events can be
	// logged for helm-operator types.
	ifscheme.AddToScheme(scheme.Scheme)
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		logger:           logger,
		logDiffs:         logReleaseDiffs,
		hrLister:         hrInformer.Lister(),
		hrSynced:         hrInformer.Informer().HasSynced,
		releaseWorkqueue: releaseWorkqueue,
		recorder:         recorder,
		release:          release,
		gitChartSync:     gitChartSync,
	}

	controller.logger.Log("info", "setting up event handlers")
	hrInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(new interface{}) {
			if _, ok := checkCustomResourceType(controller.logger, new); ok {
				releaseCount.Add(1)
				controller.enqueueJob(new)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueUpdateJob(old, new)
		},
		DeleteFunc: func(old interface{}) {
			if hr, ok := checkCustomResourceType(controller.logger, old); ok {
				releaseCount.Add(-1)
				if err := controller.release.Uninstall(hr.DeepCopy()); err != nil {
					controller.logger.Log("error", err)
				}
				status.ObserveReleaseConditions(&hr, nil)
			}
		},
	})
	controller.logger.Log("info", "event handlers set up")

	return controller
}

// Run starts workers handling the enqueued events. It will block until
// stopCh is closed, at which point it will shutdown the workqueue and
// wait for workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer runtime.HandleCrash()
	defer c.releaseWorkqueue.ShutDown()

	c.logger.Log("info", "starting operator")

	c.logger.Log("info", "starting workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	for i := 0; i < threadiness; i++ {
		wg.Done()
	}
	c.logger.Log("info", "stopping workers")
}

// runWorker is a long-running function calling the
// processNextWorkItem function to read and process a message
// on a workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	releaseQueueLength.Set(float64(c.releaseWorkqueue.Len()))

	obj, shutdown := c.releaseWorkqueue.Get()
	if shutdown {
		return false
	}

	// wrapping block in a func to defer c.workqueue.Done
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We must call Forget if we do not want
		// this work item being re-queued. If a transient error
		// occurs, we do not call Forget. Instead the item is put back
		// on the workqueue and attempted again after a back-off
		// period.
		defer c.releaseWorkqueue.Done(obj)

		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of
		// the form "namespace/hr(custom resource) name". We do this
		// as the delayed nature of the workqueue means the items in
		// the informer cache may actually be more up to date than
		// when the item was initially put onto the workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget not to get into a loop of attempting to
			// process a work item that is invalid.
			c.releaseWorkqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))

			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// HelmRelease resource to sync the corresponding Chart release.
		// If the sync failed, then we return while the item will get requeued
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("errored syncing HelmRelease '%s': %s", key, err.Error())
		}
		// If no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.releaseWorkqueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}
	return true
}

// syncHandler acts according to the action
// 		Deletes/creates or updates a Chart release
func (c *Controller) syncHandler(key string) error {
	// Retrieve namespace and Custom Resource name from the key
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.logger.Log("error", fmt.Sprintf("key '%s' is invalid: %v", key, err))
		runtime.HandleError(fmt.Errorf("key '%s' is invalid", key))
		return nil
	}

	// acquire lock
	unlock, err := c.lock(fmt.Sprintf("%s-%s", namespace, name))
	if err != nil {
		c.logger.Log("info", fmt.Sprintf("could not obtain lock: %s", err))
		return nil
	}
	defer unlock()

	// Custom Resource hr contains all information we need to know about the Chart release
	hr, err := c.hrLister.HelmReleases(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.Log("info", fmt.Sprintf("HelmRelease '%s' referred to in work queue no longer exists", key))
			runtime.HandleError(fmt.Errorf("HelmRelease '%s' referred to in work queue no longer exists", key))
			return nil
		}
		c.logger.Log("error", err.Error())
		return err
	}
	err = c.release.Sync(hr.DeepCopy())
	if err != nil {
		c.recorder.Event(hr, corev1.EventTypeWarning, FailedReleaseSync,
			fmt.Sprintf("synchronization of release '%s' in namespace '%s' failed: %s", hr.GetReleaseName(), hr.GetTargetNamespace(), err.Error()))
	} else {
		c.recorder.Event(hr, corev1.EventTypeNormal, ReleaseSynced,
			fmt.Sprintf("managed release '%s' in namespace '%s' synchronized", hr.GetReleaseName(), hr.GetTargetNamespace()))
	}
	return nil
}

func (c *Controller) lock(name string) (unlock func(), err error) {
	lockFile := path.Join(os.TempDir(), name+".lock")
	mutex := lockedfile.MutexAt(lockFile)
	return mutex.Lock()
}

// enqueueJob takes a HelmRelease resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should not be
// passed resources of any type other than HelmRelease.
func (c *Controller) enqueueJob(obj interface{}) {
	key, err := getCacheKey(obj)
	if err != nil {
		return
	}
	c.releaseWorkqueue.AddRateLimited(key)
	releaseQueueLength.Set(float64(c.releaseWorkqueue.Len()))
}

// enqueueUpdateJob decides if there is a genuine resource update
func (c *Controller) enqueueUpdateJob(old, new interface{}) {
	oldHr, ok := checkCustomResourceType(c.logger, old)
	if !ok {
		return
	}
	newHr, ok := checkCustomResourceType(c.logger, new)
	if !ok {
		return
	}

	diff := cmp.Diff(oldHr.Spec, newHr.Spec)

	// Filter out any update notifications that are due to status
	// updates, as the dry-run that determines if we should upgrade
	// is expensive, but _without_ filtering out updates that are
	// from the periodic refresh, as we still want to detect (and
	// undo) mutations to Helm charts.
	if sDiff := cmp.Diff(oldHr.Status, newHr.Status); diff == "" && sDiff != "" {
		return
	}

	// if there is a change in the chartsource (ref change, eg),
	// its possible that the mirror's ref-sha be obsolete w.r.t. upstream
	// repo's ref-sha. To avoid spurious deploy, mirror is synced before
	// doing helm reconciliation
	if csDiff := cmp.Diff(oldHr.Spec.ChartSource, newHr.Spec.ChartSource); csDiff != "" {
		c.gitChartSync.SyncMirror(&newHr)
	}

	c.enqueueJob(new)
}

func checkCustomResourceType(logger log.Logger, obj interface{}) (helmfluxv1.HelmRelease, bool) {
	var hr *helmfluxv1.HelmRelease
	var ok bool
	if hr, ok = obj.(*helmfluxv1.HelmRelease); !ok {
		logger.Log("error", fmt.Sprintf("HelmRelease Event Watch received an invalid object: %#v", obj))
		return helmfluxv1.HelmRelease{}, false
	}
	return *hr, true
}

func getCacheKey(obj interface{}) (string, error) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return "", err
	}
	return key, nil
}
