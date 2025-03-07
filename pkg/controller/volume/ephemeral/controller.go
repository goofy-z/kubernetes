/*
Copyright 2020 The Kubernetes Authors.

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

package ephemeral

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-helpers/storage/ephemeral"
	"k8s.io/kubernetes/pkg/controller/volume/common"
	ephemeralvolumemetrics "k8s.io/kubernetes/pkg/controller/volume/ephemeral/metrics"
	"k8s.io/kubernetes/pkg/controller/volume/events"
)

// Controller creates PVCs for ephemeral inline volumes in a pod spec.
type Controller interface {
	Run(workers int, stopCh <-chan struct{})
}

type ephemeralController struct {
	// kubeClient is the kube API client used by volumehost to communicate with
	// the API server.
	kubeClient clientset.Interface

	// pvcLister is the shared PVC lister used to fetch and store PVC
	// objects from the API server. It is shared with other controllers and
	// therefore the PVC objects in its store should be treated as immutable.
	pvcLister  corelisters.PersistentVolumeClaimLister
	pvcsSynced kcache.InformerSynced

	// podLister is the shared Pod lister used to fetch Pod
	// objects from the API server. It is shared with other controllers and
	// therefore the Pod objects in its store should be treated as immutable.
	podLister corelisters.PodLister
	podSynced kcache.InformerSynced

	// podIndexer has the common PodPVC indexer indexer installed To
	// limit iteration over pods to those of interest.
	podIndexer cache.Indexer

	// recorder is used to record events in the API server
	recorder record.EventRecorder

	queue workqueue.RateLimitingInterface
}

// NewController creates an ephemeral volume controller.
func NewController(
	kubeClient clientset.Interface,
	podInformer coreinformers.PodInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer) (Controller, error) {

	ec := &ephemeralController{
		kubeClient: kubeClient,
		podLister:  podInformer.Lister(),
		podIndexer: podInformer.Informer().GetIndexer(),
		podSynced:  podInformer.Informer().HasSynced,
		pvcLister:  pvcInformer.Lister(),
		pvcsSynced: pvcInformer.Informer().HasSynced,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ephemeral_volume"),
	}

	ephemeralvolumemetrics.RegisterMetrics()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	ec.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "ephemeral_volume"})

	podInformer.Informer().AddEventHandler(kcache.ResourceEventHandlerFuncs{
		AddFunc: ec.enqueuePod,
		// The pod spec is immutable. Therefore the controller can ignore pod updates
		// because there cannot be any changes that have to be copied into the generated
		// PVC.
		// Deletion of the PVC is handled through the owner reference and garbage collection.
		// Therefore pod deletions also can be ignored.
	})
	pvcInformer.Informer().AddEventHandler(kcache.ResourceEventHandlerFuncs{
		DeleteFunc: ec.onPVCDelete,
	})
	if err := common.AddPodPVCIndexerIfNotPresent(ec.podIndexer); err != nil {
		return nil, fmt.Errorf("could not initialize pvc protection controller: %w", err)
	}

	return ec, nil
}

func (ec *ephemeralController) enqueuePod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return
	}

	// Ignore pods which are already getting deleted.
	if pod.DeletionTimestamp != nil {
		return
	}

	for _, vol := range pod.Spec.Volumes {
		if vol.Ephemeral != nil {
			// It has at least one ephemeral inline volume, work on it.
			key, err := kcache.DeletionHandlingMetaNamespaceKeyFunc(pod)
			if err != nil {
				runtime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", pod, err))
				return
			}
			ec.queue.Add(key)
			break
		}
	}
}

func (ec *ephemeralController) onPVCDelete(obj interface{}) {
	pvc, ok := obj.(*v1.PersistentVolumeClaim)
	if !ok {
		return
	}

	// Someone deleted a PVC, either intentionally or
	// accidentally. If there is a pod referencing it because of
	// an ephemeral volume, then we should re-create the PVC.
	// The common indexer does some prefiltering for us by
	// limiting the list to those pods which reference
	// the PVC.
	objs, err := ec.podIndexer.ByIndex(common.PodPVCIndex, fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
	if err != nil {
		runtime.HandleError(fmt.Errorf("listing pods from cache: %v", err))
		return
	}
	for _, obj := range objs {
		ec.enqueuePod(obj)
	}
}

func (ec *ephemeralController) Run(workers int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer ec.queue.ShutDown()

	klog.Infof("Starting ephemeral volume controller")
	defer klog.Infof("Shutting down ephemeral volume controller")

	if !cache.WaitForNamedCacheSync("ephemeral", stopCh, ec.podSynced, ec.pvcsSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(ec.runWorker, time.Second, stopCh)
	}

	<-stopCh
}

func (ec *ephemeralController) runWorker() {
	for ec.processNextWorkItem() {
	}
}

func (ec *ephemeralController) processNextWorkItem() bool {
	key, shutdown := ec.queue.Get()
	if shutdown {
		return false
	}
	defer ec.queue.Done(key)

	err := ec.syncHandler(key.(string))
	if err == nil {
		ec.queue.Forget(key)
		return true
	}

	runtime.HandleError(fmt.Errorf("%v failed with: %v", key, err))
	ec.queue.AddRateLimited(key)

	return true
}

// syncHandler is invoked for each pod which might need to be processed.
// If an error is returned from this function, the pod will be requeued.
func (ec *ephemeralController) syncHandler(key string) error {
	namespace, name, err := kcache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	pod, err := ec.podLister.Pods(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(5).Infof("ephemeral: nothing to do for pod %s, it is gone", key)
			return nil
		}
		klog.V(5).Infof("Error getting pod %s/%s (uid: %q) from informer : %v", pod.Namespace, pod.Name, pod.UID, err)
		return err
	}

	// Ignore pods which are already getting deleted.
	if pod.DeletionTimestamp != nil {
		klog.V(5).Infof("ephemeral: nothing to do for pod %s, it is marked for deletion", key)
		return nil
	}

	for _, vol := range pod.Spec.Volumes {
		if err := ec.handleVolume(pod, vol); err != nil {
			ec.recorder.Event(pod, v1.EventTypeWarning, events.FailedBinding, fmt.Sprintf("ephemeral volume %s: %v", vol.Name, err))
			return fmt.Errorf("pod %s, ephemeral volume %s: %v", key, vol.Name, err)
		}
	}

	return nil
}

// handleEphemeralVolume is invoked for each volume of a pod.
func (ec *ephemeralController) handleVolume(pod *v1.Pod, vol v1.Volume) error {
	klog.V(5).Infof("ephemeral: checking volume %s", vol.Name)
	if vol.Ephemeral == nil {
		return nil
	}

	pvcName := ephemeral.VolumeClaimName(pod, &vol)
	pvc, err := ec.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(pvcName)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if pvc != nil {
		if err := ephemeral.VolumeIsForPod(pod, pvc); err != nil {
			return err
		}
		// Already created, nothing more to do.
		klog.V(5).Infof("ephemeral: volume %s: PVC %s already created", vol.Name, pvcName)
		return nil
	}

	// Create the PVC with pod as owner.
	isTrue := true
	pvc = &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Pod",
					Name:               pod.Name,
					UID:                pod.UID,
					Controller:         &isTrue,
					BlockOwnerDeletion: &isTrue,
				},
			},
			Annotations: vol.Ephemeral.VolumeClaimTemplate.Annotations,
			Labels:      vol.Ephemeral.VolumeClaimTemplate.Labels,
		},
		Spec: vol.Ephemeral.VolumeClaimTemplate.Spec,
	}
	ephemeralvolumemetrics.EphemeralVolumeCreateAttempts.Inc()
	_, err = ec.kubeClient.CoreV1().PersistentVolumeClaims(pod.Namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		ephemeralvolumemetrics.EphemeralVolumeCreateFailures.Inc()
		return fmt.Errorf("create PVC %s: %v", pvcName, err)
	}
	return nil
}
