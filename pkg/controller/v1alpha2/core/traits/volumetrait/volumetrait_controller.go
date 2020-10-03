package volumetrait

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"strings"

	cpv1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	cpmeta "github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/kubectl/pkg/util/openapi"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oamv1alpha2 "github.com/crossplane/oam-kubernetes-runtime/apis/core/v1alpha2"
	"github.com/crossplane/oam-kubernetes-runtime/pkg/oam/util"
)

// Reconcile error strings.
const (
	errQueryOpenAPI = "failed to query openAPI"
	errMountVolume  = "cannot scale the resource"
)

// Setup adds a controller that reconciles ContainerizedWorkload.
func Setup(mgr ctrl.Manager, log logging.Logger) error {
	reconcile := Reconcile{
		Client:          mgr.GetClient(),
		DiscoveryClient: *discovery.NewDiscoveryClientForConfigOrDie(mgr.GetConfig()),
		log:             ctrl.Log.WithName("VolumeTrait"),
		record:          event.NewAPIRecorder(mgr.GetEventRecorderFor("volumeTrait")),
		Scheme:          mgr.GetScheme(),
	}
	return reconcile.SetupWithManager(mgr)
}

// Reconcile reconciles a VolumeTrait object
type Reconcile struct {
	client.Client
	discovery.DiscoveryClient
	log    logr.Logger
	record event.Recorder
	Scheme *runtime.Scheme
}

//SetupWithManager to setup k8s controller.
func (r *Reconcile) SetupWithManager(mgr ctrl.Manager) error {
	name := "oam/" + strings.ToLower(oamv1alpha2.VolumeTraitKind)
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&oamv1alpha2.VolumeTrait{}).
		Complete(r)
}

// Reconcile to reconcile volume trait.
// +kubebuilder:rbac:groups=core.oam.dev,resources=volumetraits,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.oam.dev,resources=volumetraits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.oam.dev,resources=containerizedworkloads,verbs=get;list;
// +kubebuilder:rbac:groups=core.oam.dev,resources=containerizedworkloads/status,verbs=get;
// +kubebuilder:rbac:groups=core.oam.dev,resources=workloaddefinition,verbs=get;list;
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
func (r *Reconcile) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	mLog := r.log.WithValues("volume trait", req.NamespacedName)

	mLog.Info("Reconcile volume trait")

	var volumeTrait oamv1alpha2.VolumeTrait
	if err := r.Get(ctx, req.NamespacedName, &volumeTrait); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// find the resource object to record the event to, default is the parent appConfig.
	eventObj, err := util.LocateParentAppConfig(ctx, r.Client, &volumeTrait)
	if eventObj == nil {
		// fallback to workload itself
		mLog.Error(err, "Failed to find the parent resource", "volumeTrait", volumeTrait.Name)
		eventObj = &volumeTrait
	}

	// Fetch the workload instance this trait is referring to
	workload, err := util.FetchWorkload(ctx, r, mLog, &volumeTrait)
	if err != nil {
		r.record.Event(eventObj, event.Warning(util.ErrLocateWorkload, err))
		return util.ReconcileWaitResult, util.PatchCondition(
			ctx, r, &volumeTrait, cpv1alpha1.ReconcileError(errors.Wrap(err, util.ErrLocateWorkload)))
	}

	// Fetch the child resources list from the corresponding workload
	resources, err := util.FetchWorkloadChildResources(ctx, mLog, r, workload)
	if err != nil {
		mLog.Error(err, "Error while fetching the workload child resources", "workload", workload.UnstructuredContent())
		r.record.Event(eventObj, event.Warning(util.ErrFetchChildResources, err))
		return util.ReconcileWaitResult, util.PatchCondition(ctx, r, &volumeTrait,
			cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrFetchChildResources)))
	}

	// include the workload itself if there is no child resources
	if len(resources) == 0 {
		resources = append(resources, workload)
	}

	// Scale the child resources that we know how to scale
	result, err := r.mountVolume(ctx, mLog, volumeTrait, resources)
	if err != nil {
		r.record.Event(eventObj, event.Warning(errMountVolume, err))
		return result, err
	}

	r.record.Event(eventObj, event.Normal("Volume Trait applied",
		fmt.Sprintf("Trait `%s` successfully mount volume  to %v ",
			volumeTrait.Name, volumeTrait.Spec.VolumeList)))

	return ctrl.Result{}, util.PatchCondition(ctx, r, &volumeTrait, cpv1alpha1.ReconcileSuccess())
}

/*
1. 	var volumes []v1.Volume
    var pvcList []v1.PersistentVolumeClaim

2. for i, item := volumeTrait.Spec.VolumeList{
	var volumeMounts []v1.VolumeMount
	for j, v := range item.Paths{
		pvcName := fmt.Sprintf("%s-%s-%d-%d", res.GetKind(),res.GetName(), item.ContainerIndex,pathIndex)
		append(volumeMounts..)
		append(pvc...)
		append(volumes...)
	 }
   }

3. patch
patch res..container[x].volumeMounts
patch res..volumes
patch pvcList
*/
// identify child resources and add volume
func (r *Reconcile) mountVolume(ctx context.Context, mLog logr.Logger,
	volumeTrait oamv1alpha2.VolumeTrait, resources []*unstructured.Unstructured) (ctrl.Result, error) {
	isController := false
	bod := true
	// Update owner references
	ownerRef := metav1.OwnerReference{
		APIVersion:         volumeTrait.APIVersion,
		Kind:               volumeTrait.Kind,
		Name:               volumeTrait.Name,
		UID:                volumeTrait.UID,
		Controller:         &isController,
		BlockOwnerDeletion: &bod,
	}
	// prepare for openApi schema check
	schemaDoc, err := r.DiscoveryClient.OpenAPISchema()
	if err != nil {
		return util.ReconcileWaitResult,
			util.PatchCondition(ctx, r, &volumeTrait, cpv1alpha1.ReconcileError(errors.Wrap(err, errQueryOpenAPI)))
	}
	_, err = openapi.NewOpenAPIData(schemaDoc)
	if err != nil {
		return util.ReconcileWaitResult,
			util.PatchCondition(ctx, r, &volumeTrait, cpv1alpha1.ReconcileError(errors.Wrap(err, errQueryOpenAPI)))
	}

	for _, res := range resources {

		if res.GetKind() != util.KindStatefulSet && res.GetKind() != util.KindDeployment {
			continue
		}
		resPatch := client.MergeFrom(res.DeepCopyObject())
		mLog.Info("Get the resource the trait is going to modify",
			"resource name", res.GetName(), "UID", res.GetUID())
		cpmeta.AddOwnerReference(res, ownerRef)

		var volumes []interface{}
		var pvcList []v1.PersistentVolumeClaim
		for _, item := range volumeTrait.Spec.VolumeList {
			var volumeMounts []v1.VolumeMount
			for pathIndex, path := range item.Paths {
				pvcName := fmt.Sprintf("%s-%s-%d-%d", strings.ToLower(res.GetKind()), res.GetName(), item.ContainerIndex, pathIndex)
				volumes = append(volumes, v1.Volume{
					Name:         pvcName,
					VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}},
				})
				volumeMount := v1.VolumeMount{
					Name:      pvcName,
					MountPath: path.Path,
				}
				volumeMounts = append(volumeMounts, volumeMount)
				pvcList = append(pvcList, v1.PersistentVolumeClaim{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "v1",
						Kind:       util.KindPersistentVolumeClaim,
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: volumeTrait.Namespace,
					},
					Spec: v1.PersistentVolumeClaimSpec{
						StorageClassName: &path.StorageClassName,
						AccessModes: []v1.PersistentVolumeAccessMode{
							v1.ReadWriteOnce,
						},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceName(v1.ResourceStorage): resource.MustParse(path.Size),
							},
						},
					},
				})
			}

			containers, _, _ := unstructured.NestedFieldNoCopy(res.Object, "spec", "template", "spec", "containers")
			c, ok := containers.([]interface{})[item.ContainerIndex].(map[string]interface{})
			if ok {
				c["volumeMounts"] = volumeMounts
			}

		}

		spec, _, _ := unstructured.NestedFieldNoCopy(res.Object, "spec", "template", "spec")
		spec.(map[string]interface{})["volumes"] = volumes

		// merge patch to modify the pvc
		applyOpts := []client.PatchOption{client.ForceOwnership, client.FieldOwner(volumeTrait.GetUID())}
		for _, pvc := range pvcList {
			if err := r.Patch(ctx, &pvc, client.Apply, applyOpts...); err != nil {
				mLog.Error(err, "Failed to create a pvc")
				return util.ReconcileWaitResult,
					util.PatchCondition(ctx, r, &volumeTrait, cpv1alpha1.ReconcileError(errors.Wrap(err, errMountVolume)))
			}
		}

		// merge patch to modify the resource
		if err := r.Patch(ctx, res, resPatch, client.FieldOwner(volumeTrait.GetUID())); err != nil {
			mLog.Error(err, "Failed to mount volume a resource")
			return util.ReconcileWaitResult,
				util.PatchCondition(ctx, r, &volumeTrait, cpv1alpha1.ReconcileError(errors.Wrap(err, errMountVolume)))
		}

		mLog.Info("Successfully patch a resource", "resource GVK", res.GroupVersionKind().String(),
			"res UID", res.GetUID(), "target volumeClaimTemplates", volumeTrait.Spec.VolumeList)

	}
	return ctrl.Result{}, nil
}
