// Copyright 2018 The Kubeflow Authors
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

// Package controller provides a Kubernetes controller for a TFJob resource.
package tensorflow

import (
	"fmt"
	"strings"
	"time"

	kubebatchclient "github.com/kubernetes-sigs/kube-batch/pkg/client/clientset/versioned"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"

	common "github.com/kubeflow/common/job_controller/api/v1"
	"github.com/kubeflow/tf-operator/cmd/tf-operator.v1/app/options"
	tfv1 "github.com/kubeflow/tf-operator/pkg/apis/tensorflow/v1"
	tfjobclientset "github.com/kubeflow/tf-operator/pkg/client/clientset/versioned"
	tfjobscheme "github.com/kubeflow/tf-operator/pkg/client/clientset/versioned/scheme"
	tfjobinformers "github.com/kubeflow/tf-operator/pkg/client/informers/externalversions"
	tfjobinformersv1 "github.com/kubeflow/tf-operator/pkg/client/informers/externalversions/tensorflow/v1"
	tfjoblisters "github.com/kubeflow/tf-operator/pkg/client/listers/tensorflow/v1"
	"github.com/kubeflow/tf-operator/pkg/common/jobcontroller"
	tflogger "github.com/kubeflow/tf-operator/pkg/logger"
	"github.com/kubeflow/tf-operator/pkg/util/k8sutil"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	controllerName = "tf-operator"

	// labels for pods and servers.
	tfReplicaTypeLabel  = "tf-replica-type"
	tfReplicaIndexLabel = "tf-replica-index"
	labelGroupName      = "group-name"
	// Deprecated label for backwards compatibility. Has to be removed
	labelTFJobName = "tf-job-name"
)

var (
	// KeyFunc is the short name to DeletionHandlingMetaNamespaceKeyFunc.
	// IndexerInformer uses a delta queue, therefore for deletes we have to use this
	// key function but it should be just fine for non delete events.
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

	tfJobsDeletedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tf_operator_jobs_deleted_total",
		Help: "Counts number of TF jobs deleted",
	})
)

// TFController is the type for TFJob Controller, which manages
// the lifecycle of TFJobs.
type TFController struct {
	jobcontroller.JobController

	// tfJobClientSet is a clientset for CRD TFJob.
	tfJobClientSet tfjobclientset.Interface

	// To allow injection of sync functions for testing.
	syncHandler func(string) (bool, error)

	// To allow injection of updateStatus for testing.
	updateStatusHandler func(tfjob *tfv1.TFJob) error

	// To allow injection of deleteTFJob for testing.
	deleteTFJobHandler func(tfjob *tfv1.TFJob) error

	// tfJobInformer is a temporary field for unstructured informer support.
	tfJobInformer cache.SharedIndexInformer

	// Listers for TFJob, Pod and Service
	// tfJobLister can list/get tfjobs from the shared informer's store.
	tfJobLister tfjoblisters.TFJobLister

	// tfJobInformerSynced returns true if the tfjob store has been synced at least once.
	tfJobInformerSynced cache.InformerSynced
}

// NewTFController returns a new TFJob controller.
func NewTFController(
	// This variable is for unstructured informer.
	tfJobInformer tfjobinformersv1.TFJobInformer,
	kubeClientSet kubeclientset.Interface,
	kubeBatchClientSet kubebatchclient.Interface,
	tfJobClientSet tfjobclientset.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory,
	// This field is not used now but we keep it since it will be used
	// after we support CRD validation.
	tfJobInformerFactory tfjobinformers.SharedInformerFactory,
	option options.ServerOption) *TFController {

	err := tfjobscheme.AddToScheme(scheme.Scheme)
	if err != nil {
		log.Fatalf("Failed to add tfjob scheme: %v", err)
	}

	log.Info("Creating TFJob controller")
	// Create new TFController.
	tc := &TFController{
		tfJobClientSet: tfJobClientSet,
	}

	// Create base controller
	log.Info("Creating Job controller")
	jc := jobcontroller.NewJobController(tc, metav1.Duration{Duration: 15 * time.Second},
		option.EnableGangScheduling, option.GangSchedulerName, kubeClientSet, kubeBatchClientSet, kubeInformerFactory, tfv1.Plural)
	tc.JobController = jc
	// Set sync handler.
	tc.syncHandler = tc.syncTFJob
	tc.updateStatusHandler = tc.updateTFJobStatus
	// set delete handler.
	tc.deleteTFJobHandler = tc.deleteTFJob
	// Set up an event handler for when tfjob resources change.
	tfJobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    tc.addTFJob,
		UpdateFunc: tc.updateTFJob,
		// This will enter the sync loop and no-op,
		// because the tfjob has been deleted from the store.
		DeleteFunc: tc.enqueueTFJob,
	})

	tc.tfJobInformer = tfJobInformer.Informer()
	tc.tfJobLister = tfJobInformer.Lister()
	tc.tfJobInformerSynced = tfJobInformer.Informer().HasSynced

	// Create pod informer.
	podInformer := kubeInformerFactory.Core().V1().Pods()

	// Set up an event handler for when pod resources change
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    jc.AddPod,
		UpdateFunc: jc.UpdatePod,
		DeleteFunc: jc.DeletePod,
	})

	tc.PodLister = podInformer.Lister()
	tc.PodInformerSynced = podInformer.Informer().HasSynced

	// Create service informer.
	serviceInformer := kubeInformerFactory.Core().V1().Services()

	// Set up an event handler for when service resources change.
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    jc.AddService,
		UpdateFunc: jc.UpdateService,
		DeleteFunc: jc.DeleteService,
	})

	tc.ServiceLister = serviceInformer.Lister()
	tc.ServiceInformerSynced = serviceInformer.Informer().HasSynced

	return tc
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (tc *TFController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer tc.WorkQueue.ShutDown()

	// Start the informer factories to begin populating the informer caches.
	log.Info("Starting TFJob controller")

	// Wait for the caches to be synced before starting workers.
	log.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(stopCh, tc.tfJobInformerSynced,
		tc.PodInformerSynced, tc.ServiceInformerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	log.Infof("Starting %v workers", threadiness)
	// Launch workers to process TFJob resources.
	for i := 0; i < threadiness; i++ {
		go wait.Until(tc.runWorker, time.Second, stopCh)
	}

	log.Info("Started workers")
	<-stopCh
	log.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (tc *TFController) runWorker() {
	for tc.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (tc *TFController) processNextWorkItem() bool {
	obj, quit := tc.WorkQueue.Get()
	if quit {
		return false
	}
	defer tc.WorkQueue.Done(obj)

	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		// As the item in the workqueue is actually invalid, we call
		// Forget here else we'd go into a loop of attempting to
		// process a work item that is invalid.
		tc.WorkQueue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return true
	}
	logger := tflogger.LoggerForKey(key)

	tfJob, err := tc.getTFJobFromKey(key)
	if err != nil {
		if err == errNotExists {
			logger.Infof("TFJob has been deleted: %v", key)
			tfJobsDeletedCount.Inc()
			return true
		}

		// Log the failure to conditions.
		logger.Errorf("Failed to get TFJob from key %s: %v", key, err)
		if err == errFailedMarshal {
			errMsg := fmt.Sprintf("Failed to unmarshal the object to TFJob object: %v", err)
			tflogger.LoggerForJob(tfJob).Warn(errMsg)
			tc.Recorder.Event(tfJob, v1.EventTypeWarning, failedMarshalTFJobReason, errMsg)
		}

		return true
	}

	// Sync TFJob to match the actual state to this desired state.
	forget, err := tc.syncHandler(key)
	if err == nil {
		if forget {
			tc.WorkQueue.Forget(key)
		}
		return true
	}

	utilruntime.HandleError(fmt.Errorf("error syncing tfjob: %v", err))
	tc.WorkQueue.AddRateLimited(key)

	return true
}

func (tc *TFController) enqueueTFJob(tfjob interface{}) {
	key, err := KeyFunc(tfjob)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for tfjob object %#v: %v", tfjob, err))
		return
	}

	// TODO: we may need add backoff here
	tc.WorkQueue.Add(key)
}

// syncTFJob syncs the tfjob with the given key if it has had its expectations fulfilled, meaning
// it did not expect to see any more of its pods/services created or deleted.
// This function is not meant to be invoked concurrently with the same key.
func (tc *TFController) syncTFJob(key string) (bool, error) {
	startTime := time.Now()
	logger := tflogger.LoggerForKey(key)
	defer func() {
		logger.Infof("Finished syncing tfjob %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return false, err
	}
	if len(namespace) == 0 || len(name) == 0 {
		return false, fmt.Errorf("invalid tfjob key %q: either namespace or name is missing", key)
	}

	sharedTFJob, err := tc.getTFJobFromName(namespace, name)
	if err != nil {
		if err == errNotExists {
			logger.Infof("TFJob has been deleted: %v", key)
			tfJobsDeletedCount.Inc()
			// jm.expectations.DeleteExpectations(key)
			return true, nil
		}
		return false, err
	}

	tfjob := sharedTFJob.DeepCopy()

	// Sync tfjob every time if EnableDynamicWorker is true
	tfjobNeedsSync := tfjob.Spec.EnableDynamicWorker || tc.satisfiedExpectations(tfjob)

	// Set default for the new tfjob.
	scheme.Scheme.Default(tfjob)

	var reconcileTFJobsErr error
	if tfjobNeedsSync && tfjob.DeletionTimestamp == nil {
		reconcileTFJobsErr = tc.reconcileTFJobs(tfjob)
	}

	if reconcileTFJobsErr != nil {
		return false, reconcileTFJobsErr
	}

	return true, err
}

// reconcileTFJobs checks and updates replicas for each given TFReplicaSpec.
// It will requeue the tfjob in case of an error while creating/deleting pods/services.
func (tc *TFController) reconcileTFJobs(tfjob *tfv1.TFJob) error {
	tfjobKey, err := KeyFunc(tfjob)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for tfjob object %#v: %v", tfjob, err))
		return err
	}
	logger := tflogger.LoggerForJob(tfjob)
	logger.Infof("Reconcile TFJobs %s", tfjob.Name)

	oldStatus := tfjob.Status.DeepCopy()

	pods, err := tc.GetPodsForJob(tfjob)

	if err != nil {
		logger.Warnf("getPodsForTFJob error %v", err)
		return err
	}

	services, err := tc.GetServicesForJob(tfjob)

	if err != nil {
		logger.Warnf("getServicesForTFJob error %v", err)
		return err
	}

	// If the TFJob is terminated, delete all pods and services.
	if isSucceeded(tfjob.Status) || isFailed(tfjob.Status) {
		if err := tc.deletePodsAndServices(tfjob, pods); err != nil {
			return err
		}

		if err := tc.cleanupTFJob(tfjob); err != nil {
			return err
		}

		if tc.Config.EnableGangScheduling {
			if err := tc.DeletePodGroup(tfjob); err != nil {
				return err
			}
		}

		// At this point the pods may have been deleted, so if the job succeeded, we need to manually set the replica status.
		// If any replicas are still Active, set their status to succeeded.
		if isSucceeded(tfjob.Status) {
			for rtype := range tfjob.Status.ReplicaStatuses {
				tfjob.Status.ReplicaStatuses[rtype].Succeeded += tfjob.Status.ReplicaStatuses[rtype].Active
				tfjob.Status.ReplicaStatuses[rtype].Active = 0
			}
		}
		// no need to update the tfjob if the status hasn't changed since last time even the tfjob is not running.

		if !apiequality.Semantic.DeepEqual(*oldStatus, tfjob.Status) {
			return tc.updateStatusHandler(tfjob)
		}
		return nil
	}

	// retrieve the previous number of retry
	previousRetry := tc.WorkQueue.NumRequeues(tfjobKey)

	activePods := k8sutil.FilterActivePods(pods)
	active := int32(len(activePods))
	failed := k8sutil.FilterPodCount(pods, v1.PodFailed)
	totalReplicas := getTotalReplicas(tfjob)
	prevReplicasFailedNum := getTotalFailedReplicas(tfjob)

	var failureMessage string
	tfJobExceedsLimit := false
	exceedsBackoffLimit := false
	pastBackoffLimit := false

	if tfjob.Spec.BackoffLimit != nil {
		jobHasNewFailure := failed > prevReplicasFailedNum
		// new failures happen when status does not reflect the failures and active
		// is different than parallelism, otherwise the previous controller loop
		// failed updating status so even if we pick up failure it is not a new one
		exceedsBackoffLimit = jobHasNewFailure && (active != totalReplicas) &&
			(int32(previousRetry)+1 > *tfjob.Spec.BackoffLimit)

		pastBackoffLimit, err = tc.pastBackoffLimit(tfjob, pods)
		if err != nil {
			return err
		}
	}

	if exceedsBackoffLimit || pastBackoffLimit {
		// check if the number of pod restart exceeds backoff (for restart OnFailure only)
		// OR if the number of failed jobs increased since the last syncJob
		tfJobExceedsLimit = true
		failureMessage = fmt.Sprintf("TFJob %s has failed because it has reached the specified backoff limit", tfjob.Name)
	} else if tc.pastActiveDeadline(tfjob) {
		failureMessage = fmt.Sprintf("TFJob %s has failed because it was active longer than specified deadline", tfjob.Name)
		tfJobExceedsLimit = true
	}

	if tfJobExceedsLimit {
		// If the TFJob exceeds backoff limit or is past active deadline
		// delete all pods and services, then set the status to failed
		if err := tc.deletePodsAndServices(tfjob, pods); err != nil {
			return err
		}

		if err := tc.cleanupTFJob(tfjob); err != nil {
			return err
		}

		if tc.Config.EnableGangScheduling {
			if err := tc.DeletePodGroup(tfjob); err != nil {
				return err
			}
		}

		tc.Recorder.Event(tfjob, v1.EventTypeNormal, tfJobFailedReason, failureMessage)
		if tfjob.Status.CompletionTime == nil {
			now := metav1.Now()
			tfjob.Status.CompletionTime = &now
		}
		if err := updateTFJobConditions(
			tfjob, common.JobFailed, tfJobFailedReason, failureMessage); err != nil {
			tflogger.LoggerForJob(tfjob).Infof("Append tfjob condition error: %v", err)
			return err
		}
	} else {
		if tc.Config.EnableGangScheduling {
			minAvailableReplicas := getTotalReplicas(tfjob)
			_, err := tc.SyncPodGroup(tfjob, minAvailableReplicas)
			if err != nil {
				logger.Warnf("Sync PodGroup %v: %v", tfjob.Name, err)
			}
		}

		// Save the current state of the replicas
		replicasStatus := make(map[string]v1.PodPhase)

		// Diff current active pods/services with replicas.
		for rtype, spec := range tfjob.Spec.TFReplicaSpecs {
			err = tc.reconcilePods(tfjob, pods, rtype, spec, replicasStatus)
			if err != nil {
				logger.Warnf("reconcilePods error %v", err)
				return err
			}

			err = tc.reconcileServices(tfjob, services, rtype, spec)

			if err != nil {
				logger.Warnf("reconcileServices error %v", err)
				return err
			}
		}
	}

	// no need to update the tfjob if the status hasn't changed since last time.
	if !apiequality.Semantic.DeepEqual(*oldStatus, tfjob.Status) {
		return tc.updateStatusHandler(tfjob)
	}
	return nil
}

// satisfiedExpectations returns true if the required adds/dels for the given tfjob have been observed.
// Add/del counts are established by the controller at sync time, and updated as controllees are observed by the controller
// manager.
func (tc *TFController) satisfiedExpectations(tfjob *tfv1.TFJob) bool {
	satisfied := true
	tfjobKey, err := KeyFunc(tfjob)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for tfjob object %#v: %v", tfjob, err))
		return false
	}

	for rtype := range tfjob.Spec.TFReplicaSpecs {
		// Check the expectations of the pods.
		expectationPodsKey := jobcontroller.GenExpectationPodsKey(tfjobKey, string(rtype))
		satisfied = satisfied && tc.Expectations.SatisfiedExpectations(expectationPodsKey)

		// Check the expectations of the services.
		expectationServicesKey := jobcontroller.GenExpectationServicesKey(tfjobKey, string(rtype))
		satisfied = satisfied && tc.Expectations.SatisfiedExpectations(expectationServicesKey)
	}

	return satisfied
}

// pastBackoffLimit checks if container restartCounts sum exceeds BackoffLimit
// this method applies only to pods with restartPolicy == OnFailure or Always
func (tc *TFController) pastBackoffLimit(tfjob *tfv1.TFJob, pods []*v1.Pod) (bool, error) {
	if tfjob.Spec.BackoffLimit == nil {
		return false, nil
	}
	logger := tflogger.LoggerForJob(tfjob)
	result := int32(0)
	for rtype, spec := range tfjob.Spec.TFReplicaSpecs {
		if spec.RestartPolicy != common.RestartPolicyOnFailure && spec.RestartPolicy != common.RestartPolicyAlways {
			logger.Warnf("The restart policy of replica %v of the job %v is not OnFailure or Always. Not counted in backoff limit.", rtype, tfjob.Name)
			continue
		}
		// Convert TFReplicaType to lower string.
		rt := strings.ToLower(string(rtype))
		pods, err := tc.FilterPodsForReplicaType(pods, rt)
		if err != nil {
			return false, err
		}
		for i := range pods {
			po := pods[i]
			if po.Status.Phase == v1.PodRunning || po.Status.Phase == v1.PodPending {
				for j := range po.Status.InitContainerStatuses {
					stat := po.Status.InitContainerStatuses[j]
					result += stat.RestartCount
				}
				for j := range po.Status.ContainerStatuses {
					stat := po.Status.ContainerStatuses[j]
					result += stat.RestartCount
				}
			}
		}
	}

	if *tfjob.Spec.BackoffLimit == 0 {
		return result > 0, nil
	}
	return result >= *tfjob.Spec.BackoffLimit, nil
}

// pastActiveDeadline checks if job has ActiveDeadlineSeconds field set and if it is exceeded.
func (tc *TFController) pastActiveDeadline(tfjob *tfv1.TFJob) bool {
	if tfjob.Spec.ActiveDeadlineSeconds == nil || tfjob.Status.StartTime == nil {
		return false
	}
	now := metav1.Now()
	start := tfjob.Status.StartTime.Time
	duration := now.Time.Sub(start)
	allowedDuration := time.Duration(*tfjob.Spec.ActiveDeadlineSeconds) * time.Second
	return duration >= allowedDuration
}

func (tc *TFController) GetJobFromInformerCache(namespace, name string) (metav1.Object, error) {
	return tc.getTFJobFromName(namespace, name)
}

func (tc *TFController) GetJobFromAPIClient(namespace, name string) (metav1.Object, error) {
	return tc.tfJobClientSet.KubeflowV1().TFJobs(namespace).Get(name, metav1.GetOptions{})
}

func (tc *TFController) GetAPIGroupVersionKind() schema.GroupVersionKind {
	return tfv1.SchemeGroupVersionKind
}

func (tc *TFController) GetAPIGroupVersion() schema.GroupVersion {
	return tfv1.SchemeGroupVersion
}

func (tc *TFController) GetGroupNameLabelKey() string {
	return labelGroupName
}

// Deprecated function for backwards compatibility. Has to be removed later
func (tc *TFController) GetJobNameLabelKey() string {
	return labelTFJobName
}

func (tc *TFController) GetGroupNameLabelValue() string {
	return tfv1.GroupName
}

func (tc *TFController) GetReplicaTypeLabelKey() string {
	return tfReplicaTypeLabel
}

func (tc *TFController) GetReplicaIndexLabelKey() string {
	return tfReplicaIndexLabel
}

func (tc *TFController) ControllerName() string {
	return controllerName
}
