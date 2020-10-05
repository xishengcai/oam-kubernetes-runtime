/*

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

package containerizedworkload

import (
	"context"
	"fmt"
	"strings"

	cpv1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/crossplane/oam-kubernetes-runtime/apis/core/v1alpha2"
	"github.com/crossplane/oam-kubernetes-runtime/pkg/oam/util"
)

// Reconcile error strings.
const (
	errRenderWorkload     = "cannot render workload"
	errApplyDeployment    = "cannot apply the deployment"
	errApplyStatefulSet   = "cannot apply the statefulSet"
)

const (
	deployment  = "deployment"
	statefulSet = "statefulSet"
	dynamic     = "dynamic"
)

// Setup adds a controller that reconciles ContainerizedWorkload.
func Setup(mgr ctrl.Manager, log logging.Logger) error {
	reconciler := Reconciler{
		Client: mgr.GetClient(),
		log:    ctrl.Log.WithName("ContainerizedWorkload"),
		record: event.NewAPIRecorder(mgr.GetEventRecorderFor("ContainerizedWorkload")),
		Scheme: mgr.GetScheme(),
	}
	return reconciler.SetupWithManager(mgr)
}

// Reconciler reconciles a ContainerizedWorkload object
type Reconciler struct {
	client.Client
	log           logr.Logger
	record        event.Recorder
	Scheme        *runtime.Scheme
	childResource []string
}

// Reconcile reconciles a ContainerizedWorkload object
// +kubebuilder:rbac:groups=core.oam.dev,resources=containerizedworkloads,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.oam.dev,resources=containerizedworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.log.WithValues("containerizedworkload", req.NamespacedName)
	log.Info("Reconcile container workload")

	var workload v1alpha2.ContainerizedWorkload
	if err := r.Get(ctx, req.NamespacedName, &workload); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Container workload is deleted")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	//log.Info("Get the workload", "apiVersion", workload.APIVersion, "kind", workload.Kind)
	// find the resource object to record the event to, default is the parent appConfig.

	eventObj, err := util.LocateParentAppConfig(ctx, r.Client, &workload)
	if eventObj == nil {
		// fallback to workload itself
		log.Error(err, "workload", workload.Name)
		eventObj = &workload
	}

	// applicationConfiguration write label by workload child define
	if workload.Labels == nil {
		log.Info("not define child resource")
		return ctrl.Result{}, nil
	}

	childResource := workload.Labels[util.LabelKeyChildResource]

	//r.childResource = childResourceValue
	workload.Status.Resources = nil
	if childResource == util.KindStatefulSet {
		sts, err := r.renderStatefulSet(ctx, &workload)
		if err != nil {
			log.Error(err, "Failed to render a statefulSet")
			r.record.Event(eventObj, event.Warning(errRenderWorkload, err))
			return util.ReconcileWaitResult,
				util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errRenderWorkload)))
		}
		// server side apply, only the fields we set are touched
		applyOpts := []client.PatchOption{client.ForceOwnership, client.FieldOwner(workload.GetUID())}
		if err := r.Patch(ctx, sts, client.Apply, applyOpts...); err != nil {
			log.Error(err, "Failed to apply to a statefulSet")
			r.record.Event(eventObj, event.Warning(errApplyStatefulSet, err))
			return util.ReconcileWaitResult,
				util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errApplyStatefulSet)))
		}
		workload.Status.Resources = append(workload.Status.Resources,
			cpv1alpha1.TypedReference{
				APIVersion: sts.GetObjectKind().GroupVersionKind().GroupVersion().String(),
				Kind:       sts.GetObjectKind().GroupVersionKind().Kind,
				Name:       sts.GetName(),
				UID:        sts.UID,
			},
		)
		r.record.Event(eventObj, event.Normal("StatefulSet created",
			fmt.Sprintf("Workload `%s` successfully server side patched a statefulSet `%s`", workload.Name, sts.Name)))

		// garbage collect the statefulSet that we created but not needed
		if err := r.cleanupResources(ctx, &workload, util.KindStatefulSet, sts.UID); err != nil {
			log.Error(err, "Failed to clean up resources")
			r.record.Event(eventObj, event.Warning(errApplyStatefulSet, err))
		}
	} else {
		deploy, err := r.renderDeployment(ctx, &workload)
		if err != nil {
			log.Error(err, "Failed to render a deployment")
			r.record.Event(eventObj, event.Warning(errRenderWorkload, err))
			return util.ReconcileWaitResult,
				util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errRenderWorkload)))
		}
		// server side apply, only the fields we set are touched
		applyOpts := []client.PatchOption{client.ForceOwnership, client.FieldOwner(workload.GetUID())}
		if err := r.Patch(ctx, deploy, client.Apply, applyOpts...); err != nil {
			log.Error(err, "Failed to apply to a deployment")
			r.record.Event(eventObj, event.Warning(errApplyDeployment, err))
			return util.ReconcileWaitResult,
				util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileError(errors.Wrap(err, errApplyDeployment)))
		}
		workload.Status.Resources = append(workload.Status.Resources,
			cpv1alpha1.TypedReference{
				APIVersion: deploy.GetObjectKind().GroupVersionKind().GroupVersion().String(),
				Kind:       deploy.GetObjectKind().GroupVersionKind().Kind,
				Name:       deploy.GetName(),
				UID:        deploy.UID,
			},
		)
		r.record.Event(eventObj, event.Normal("Deployment created",
			fmt.Sprintf("Workload `%s` successfully server side patched a deployment `%s`",
				workload.Name, deploy.Name)))

		// garbage collect the deployment that we created but not needed
		if err := r.cleanupResources(ctx, &workload, util.KindDeployment, deploy.UID); err != nil {
			log.Error(err, "Failed to clean up resources")
			r.record.Event(eventObj, event.Warning(errApplyDeployment, err))
		}
	}
	if err := r.Status().Update(ctx, &workload); err != nil {
		return util.ReconcileWaitResult, err
	}
	return ctrl.Result{}, util.PatchCondition(ctx, r, &workload, cpv1alpha1.ReconcileSuccess())
}

//SetupWithManager setups up k8s controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	src := &v1alpha2.ContainerizedWorkload{}
	name := "oam/" + strings.ToLower(v1alpha2.ContainerizedWorkloadKind)
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(src).
		Owns(&appsv1.Deployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Service{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.StatefulSet{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
