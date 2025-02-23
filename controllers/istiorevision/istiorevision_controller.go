// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package istiorevision

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"maistra.io/istio-operator/api/v1alpha1"
	"maistra.io/istio-operator/pkg/helm"
	"maistra.io/istio-operator/pkg/kube"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"
	"istio.io/istio/pkg/ptr"
)

const (
	cniReleaseNameBase         = "istio"
	IstioInjectionLabel        = "istio-injection"
	IstioInjectionEnabledValue = "enabled"
	IstioRevLabel              = "istio.io/rev"
	IstioSidecarInjectLabel    = "sidecar.istio.io/inject"
)

// IstioRevisionReconciler reconciles an IstioRevision object
type IstioRevisionReconciler struct {
	CNINamespace     string
	RestClientGetter genericclioptions.RESTClientGetter
	client.Client
	Scheme *runtime.Scheme
}

func NewIstioRevisionReconciler(client client.Client, scheme *runtime.Scheme, restConfig *rest.Config, cniNamespace string) *IstioRevisionReconciler {
	return &IstioRevisionReconciler{
		CNINamespace:     cniNamespace,
		RestClientGetter: helm.NewRESTClientGetter(restConfig),
		Client:           client,
		Scheme:           scheme,
	}
}

// charts to deploy in the istio namespace
var userCharts = []string{"istiod"}

// +kubebuilder:rbac:groups=operator.istio.io,resources=istiorevisions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.istio.io,resources=istiorevisions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=operator.istio.io,resources=istiorevisions/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources="*",verbs="*"
// +kubebuilder:rbac:groups="networking.k8s.io",resources="networkpolicies",verbs="*"
// +kubebuilder:rbac:groups="policy",resources="poddisruptionbudgets",verbs="*"
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs="*"
// +kubebuilder:rbac:groups="apps",resources=deployments;daemonsets,verbs="*"
// +kubebuilder:rbac:groups="admissionregistration.k8s.io",resources=validatingwebhookconfigurations;mutatingwebhookconfigurations,verbs="*"
// +kubebuilder:rbac:groups="autoscaling",resources=horizontalpodautoscalers,verbs="*"
// +kubebuilder:rbac:groups="apiextensions.k8s.io",resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups="k8s.cni.cncf.io",resources=network-attachment-definitions,verbs="*"
// +kubebuilder:rbac:groups="security.openshift.io",resources=securitycontextconstraints,resourceNames=privileged,verbs=use
// +kubebuilder:rbac:groups="networking.istio.io",resources=envoyfilters,verbs="*"

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *IstioRevisionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var rev v1alpha1.IstioRevision
	if err := r.Client.Get(ctx, req.NamespacedName, &rev); err != nil {
		if errors.IsNotFound(err) {
			log.V(2).Info("IstioRevision not found. Skipping reconciliation")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get IstioRevision from cluster")
	}

	if rev.DeletionTimestamp != nil {
		if err := r.uninstallHelmCharts(ctx, &rev); err != nil {
			return ctrl.Result{}, err
		}

		if err := kube.RemoveFinalizer(ctx, &rev, r.Client); err != nil {
			log.Info("failed to remove finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !kube.HasFinalizer(&rev) {
		err := kube.AddFinalizer(ctx, &rev, r.Client)
		if err != nil {
			log.Info("failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	if err := validateIstioRevision(rev); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Installing components")
	err := r.installHelmCharts(ctx, &rev)

	log.Info("Reconciliation done. Updating status.")
	err = r.updateStatus(ctx, &rev, err)

	return ctrl.Result{}, err
}

func validateIstioRevision(rev v1alpha1.IstioRevision) error {
	if rev.Spec.Version == "" {
		return fmt.Errorf("spec.version not set")
	}
	if rev.Spec.Namespace == "" {
		return fmt.Errorf("spec.namespace not set")
	}
	if rev.Spec.Values == nil {
		return fmt.Errorf("spec.values not set")
	}

	if rev.Name == v1alpha1.DefaultRevision && rev.Spec.Values.Revision != "" {
		return fmt.Errorf("spec.values.revision must be \"\" when IstioRevision name is %s", v1alpha1.DefaultRevision)
	} else if rev.Name != v1alpha1.DefaultRevision && rev.Spec.Values.Revision != rev.Name {
		return fmt.Errorf("spec.values.revision does not match IstioRevision name")
	}

	if rev.Spec.Values.Global == nil || rev.Spec.Values.Global.IstioNamespace != rev.Spec.Namespace {
		return fmt.Errorf("spec.values.global.istioNamespace does not match spec.namespace")
	}
	return nil
}

func (r *IstioRevisionReconciler) installHelmCharts(ctx context.Context, rev *v1alpha1.IstioRevision) error {
	ownerReference := metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               v1alpha1.IstioRevisionKind,
		Name:               rev.Name,
		UID:                rev.UID,
		Controller:         ptr.Of(true),
		BlockOwnerDeletion: ptr.Of(true),
	}

	values := rev.Spec.Values.ToHelmValues()

	if isCNIEnabled(rev.Spec.Values) {
		if shouldInstallCNI, err := r.isOldestRevisionWithCNI(ctx, rev); shouldInstallCNI {
			if err := helm.UpgradeOrInstallCharts(ctx, r.RestClientGetter, []string{"cni"}, values,
				rev.Spec.Version, cniReleaseNameBase, r.CNINamespace, ownerReference); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			log := logf.FromContext(ctx)
			log.Info("Skipping istio-cni-node installation because CNI is already installed and owned by another IstioRevision")
		}
	}

	if err := helm.UpgradeOrInstallCharts(ctx, r.RestClientGetter, userCharts, values,
		rev.Spec.Version, rev.Name, rev.Spec.Namespace, ownerReference); err != nil {
		return err
	}
	return nil
}

func (r *IstioRevisionReconciler) uninstallHelmCharts(ctx context.Context, rev *v1alpha1.IstioRevision) error {
	if err := helm.UninstallCharts(ctx, r.RestClientGetter, []string{"cni"}, cniReleaseNameBase, r.CNINamespace); err != nil {
		return err
	}

	if err := helm.UninstallCharts(ctx, r.RestClientGetter, userCharts, rev.Name, rev.Spec.Namespace); err != nil {
		return err
	}
	return nil
}

func (r *IstioRevisionReconciler) isOldestRevisionWithCNI(ctx context.Context, rev *v1alpha1.IstioRevision) (bool, error) {
	revList := v1alpha1.IstioRevisionList{}
	if err := r.Client.List(ctx, &revList); err != nil {
		return false, err
	}

	oldestRevision := *rev
	for _, item := range revList.Items {
		if isCNIEnabled(item.Spec.Values) &&
			(item.CreationTimestamp.Before(&oldestRevision.CreationTimestamp) ||
				item.CreationTimestamp.Equal(&oldestRevision.CreationTimestamp) &&
					strings.Compare(item.Name, oldestRevision.Name) < 0) {

			oldestRevision = item
		}
	}
	return oldestRevision.UID == rev.UID, nil
}

func isCNIEnabled(values *v1alpha1.Values) bool {
	if values == nil {
		return false
	}
	// TODO: remove this temporary hack when we introduce Istio.spec.components.cni.enabled
	if values.Profile == "openshift" {
		return true
	}

	return values.IstioCni != nil && values.IstioCni.Enabled
}

// SetupWithManager sets up the controller with the Manager.
func (r *IstioRevisionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// ownedResourceHandler handles resources that are owned by the IstioRevision CR
	ownedResourceHandler := handler.EnqueueRequestsFromMapFunc(r.mapOwnerToReconcileRequest)

	// nsHandler handles namespaces that reference the IstioRevision CR via the istio.io/rev or istio-injection labels.
	// The handler triggers the reconciliation of the referenced IstioRevision CR so that its InUse condition is updated.
	nsHandler := handler.EnqueueRequestsFromMapFunc(r.mapNamespaceToReconcileRequest)

	// podHandler handles pods that reference the IstioRevision CR via the istio.io/rev or sidecar.istio.io/inject labels.
	// The handler triggers the reconciliation of the referenced IstioRevision CR so that its InUse condition is updated.
	podHandler := handler.EnqueueRequestsFromMapFunc(r.mapPodToReconcileRequest)

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			LogConstructor: func(req *reconcile.Request) logr.Logger {
				log := mgr.GetLogger().WithName("ctrlr").WithName("istiorev")
				if req != nil {
					log = log.WithValues("IstioRevision", req.Name)
				}
				return log
			},
		}).
		For(&v1alpha1.IstioRevision{}).

		// namespaced resources
		Watches(&corev1.ConfigMap{}, ownedResourceHandler).
		Watches(&appsv1.Deployment{}, ownedResourceHandler).
		Watches(&appsv1.DaemonSet{}, ownedResourceHandler).
		Watches(&corev1.Endpoints{}, ownedResourceHandler).
		Watches(&corev1.ResourceQuota{}, ownedResourceHandler).
		Watches(&corev1.Secret{}, ownedResourceHandler).
		Watches(&corev1.Service{}, ownedResourceHandler).
		Watches(&corev1.ServiceAccount{}, ownedResourceHandler).
		Watches(&rbacv1.Role{}, ownedResourceHandler).
		Watches(&rbacv1.RoleBinding{}, ownedResourceHandler).
		Watches(&policyv1.PodDisruptionBudget{}, ownedResourceHandler).
		Watches(&autoscalingv2.HorizontalPodAutoscaler{}, ownedResourceHandler).
		Watches(&networkingv1alpha3.EnvoyFilter{}, ownedResourceHandler).

		// TODO: only register NetAttachDef if the CRD is installed (may also need to watch for CRD creation)
		// Owns(&multusv1.NetworkAttachmentDefinition{}).

		Watches(&corev1.Namespace{}, nsHandler).
		Watches(&corev1.Pod{}, podHandler).

		// cluster-scoped resources
		Watches(&rbacv1.ClusterRole{}, ownedResourceHandler).
		Watches(&rbacv1.ClusterRoleBinding{}, ownedResourceHandler).
		Watches(&admissionv1.MutatingWebhookConfiguration{}, ownedResourceHandler).
		Watches(&admissionv1.ValidatingWebhookConfiguration{},
			ownedResourceHandler,
			builder.WithPredicates(validatingWebhookConfigPredicate{})).

		// +lint-watches:ignore: CustomResourceDefinition (prevents `make lint-watches` from bugging us about CRDs)
		Complete(r)
}

func (r *IstioRevisionReconciler) updateStatus(ctx context.Context, rev *v1alpha1.IstioRevision, err error) error {
	log := logf.FromContext(ctx)
	reconciledCondition := r.determineReconciledCondition(err)
	readyCondition := r.determineReadyCondition(ctx, rev)
	inUseCondition, err := r.determineInUseCondition(ctx, rev)
	if err != nil {
		return err
	}

	status := rev.Status.DeepCopy()
	status.ObservedGeneration = rev.Generation
	status.SetCondition(reconciledCondition)
	status.SetCondition(readyCondition)
	status.SetCondition(inUseCondition)
	status.State = deriveState(reconciledCondition, readyCondition)

	if reflect.DeepEqual(rev.Status, *status) {
		return nil
	}

	statusErr := r.Client.Status().Patch(ctx, rev, kube.NewStatusPatch(*status))
	if statusErr != nil {
		log.Error(statusErr, "failed to patch status")

		// ensure that we retry the reconcile by returning the status error
		// (but without overriding the original error)
		if err == nil {
			return statusErr
		}
	}
	return err
}

func deriveState(reconciledCondition, readyCondition v1alpha1.IstioRevisionCondition) v1alpha1.IstioRevisionConditionReason {
	if reconciledCondition.Status == metav1.ConditionFalse {
		return reconciledCondition.Reason
	} else if readyCondition.Status == metav1.ConditionFalse {
		return readyCondition.Reason
	}

	return v1alpha1.IstioRevisionConditionReasonHealthy
}

func (r *IstioRevisionReconciler) determineReconciledCondition(err error) v1alpha1.IstioRevisionCondition {
	if err == nil {
		return v1alpha1.IstioRevisionCondition{
			Type:   v1alpha1.IstioRevisionConditionTypeReconciled,
			Status: metav1.ConditionTrue,
		}
	}

	return v1alpha1.IstioRevisionCondition{
		Type:    v1alpha1.IstioRevisionConditionTypeReconciled,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.IstioRevisionConditionReasonReconcileError,
		Message: fmt.Sprintf("error reconciling resource: %v", err),
	}
}

func (r *IstioRevisionReconciler) determineReadyCondition(ctx context.Context, rev *v1alpha1.IstioRevision) v1alpha1.IstioRevisionCondition {
	notReady := func(reason v1alpha1.IstioRevisionConditionReason, message string) v1alpha1.IstioRevisionCondition {
		return v1alpha1.IstioRevisionCondition{
			Type:    v1alpha1.IstioRevisionConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		}
	}

	istiod := appsv1.Deployment{}
	if err := r.Client.Get(ctx, istiodDeploymentKey(rev), &istiod); err != nil {
		if errors.IsNotFound(err) {
			return notReady(v1alpha1.IstioRevisionConditionReasonIstiodNotReady, "istiod Deployment not found")
		}
		return notReady(v1alpha1.IstioRevisionConditionReasonReconcileError, fmt.Sprintf("failed to get readiness: %v", err))
	}
	if istiod.Status.Replicas == 0 {
		return notReady(v1alpha1.IstioRevisionConditionReasonIstiodNotReady, "istiod Deployment is scaled to zero replicas")
	} else if istiod.Status.ReadyReplicas < istiod.Status.Replicas {
		return notReady(v1alpha1.IstioRevisionConditionReasonIstiodNotReady, "not all istiod pods are ready")
	}

	if isCNIEnabled(rev.Spec.Values) {
		cni := appsv1.DaemonSet{}
		if err := r.Client.Get(ctx, r.cniDaemonSetKey(), &cni); err != nil {
			if errors.IsNotFound(err) {
				return notReady(v1alpha1.IstioRevisionConditionReasonCNINotReady, "istio-cni-node DaemonSet not found")
			}
			return notReady(v1alpha1.IstioRevisionConditionReasonReconcileError, fmt.Sprintf("failed to get readiness: %v", err))
		}

		if cni.Status.CurrentNumberScheduled == 0 {
			return notReady(v1alpha1.IstioRevisionConditionReasonCNINotReady, "no istio-cni-node pods are currently scheduled")
		} else if cni.Status.NumberReady < cni.Status.CurrentNumberScheduled {
			return notReady(v1alpha1.IstioRevisionConditionReasonCNINotReady, "not all istio-cni-node pods are ready")
		}
	}

	return v1alpha1.IstioRevisionCondition{
		Type:   v1alpha1.IstioRevisionConditionTypeReady,
		Status: metav1.ConditionTrue,
	}
}

func (r *IstioRevisionReconciler) determineInUseCondition(ctx context.Context, rev *v1alpha1.IstioRevision) (v1alpha1.IstioRevisionCondition, error) {
	isReferenced, err := r.isRevisionReferencedByWorkloads(ctx, rev)
	if err != nil {
		return v1alpha1.IstioRevisionCondition{}, err
	}

	if isReferenced {
		return v1alpha1.IstioRevisionCondition{
			Type:    v1alpha1.IstioRevisionConditionTypeInUse,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.IstioRevisionConditionReasonReferencedByWorkloads,
			Message: "Referenced by at least one pod or namespace",
		}, nil
	}
	return v1alpha1.IstioRevisionCondition{
		Type:    v1alpha1.IstioRevisionConditionTypeInUse,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.IstioRevisionConditionReasonNotReferenced,
		Message: "Not referenced by any pod or namespace",
	}, nil
}

func (r *IstioRevisionReconciler) isRevisionReferencedByWorkloads(ctx context.Context, rev *v1alpha1.IstioRevision) (bool, error) {
	log := logf.FromContext(ctx)
	nsList := corev1.NamespaceList{}
	nsMap := map[string]corev1.Namespace{}
	if err := r.Client.List(ctx, &nsList); err != nil { // TODO: can we optimize this by specifying a label selector
		return false, err
	}
	for _, ns := range nsList.Items {
		if namespaceReferencesRevision(ns, rev) {
			log.V(2).Info("Revision is referenced by Namespace", "Namespace", ns.Name)
			return true, nil
		}
		nsMap[ns.Name] = ns
	}

	podList := corev1.PodList{}
	if err := r.Client.List(ctx, &podList); err != nil { // TODO: can we optimize this by specifying a label selector
		return false, err
	}
	for _, pod := range podList.Items {
		if ns, found := nsMap[pod.Namespace]; found && podReferencesRevision(pod, ns, rev) {
			log.V(2).Info("Revision is referenced by Pod", "Pod", client.ObjectKeyFromObject(&pod))
			return true, nil
		}
	}

	if rev.Name == v1alpha1.DefaultRevision && rev.Spec.Values != nil &&
		rev.Spec.Values.SidecarInjectorWebhook != nil &&
		rev.Spec.Values.SidecarInjectorWebhook.EnableNamespacesByDefault {
		return true, nil
	}

	log.V(2).Info("Revision is not referenced by any Pod or Namespace")
	return false, nil
}

func namespaceReferencesRevision(ns corev1.Namespace, rev *v1alpha1.IstioRevision) bool {
	return rev.Name == getReferencedRevisionFromNamespace(ns.Labels)
}

func podReferencesRevision(pod corev1.Pod, ns corev1.Namespace, rev *v1alpha1.IstioRevision) bool {
	return rev.Name == getReferencedRevisionFromPod(pod.GetLabels(), pod.GetAnnotations(), ns.GetLabels())
}

func getReferencedRevisionFromNamespace(labels map[string]string) string {
	if labels[IstioInjectionLabel] == IstioInjectionEnabledValue {
		return v1alpha1.DefaultRevision
	}
	revision := labels[IstioRevLabel]
	if revision != "" {
		return revision
	}
	// TODO: if .Values.sidecarInjectorWebhook.enableNamespacesByDefault is true, then all namespaces except system namespaces should use the "default" revision

	return ""
}

func getReferencedRevisionFromPod(podLabels, podAnnotations, nsLabels map[string]string) string {
	// if pod was already injected, the revision that did the injection is specified in the istio.io/rev annotation
	revision := podAnnotations[IstioRevLabel]
	if revision != "" {
		return revision
	}

	// pod is marked for injection by a specific revision, but wasn't injected (e.g. because it was created before the revision was applied)
	revisionFromNamespace := getReferencedRevisionFromNamespace(nsLabels)
	if podLabels[IstioSidecarInjectLabel] != "false" {
		if revisionFromNamespace != "" {
			return revisionFromNamespace
		}
		revisionFromPod := podLabels[IstioRevLabel]
		if revisionFromPod != "" {
			return revisionFromPod
		} else if podLabels[IstioSidecarInjectLabel] == "true" {
			return v1alpha1.DefaultRevision
		}
	}
	return ""
}

func (r *IstioRevisionReconciler) cniDaemonSetKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: r.CNINamespace,
		Name:      "istio-cni-node",
	}
}

func istiodDeploymentKey(rev *v1alpha1.IstioRevision) client.ObjectKey {
	name := "istiod"
	if rev.Spec.Values != nil && rev.Spec.Values.Revision != "" {
		name += "-" + rev.Spec.Values.Revision
	}

	return client.ObjectKey{
		Namespace: rev.Spec.Namespace,
		Name:      name,
	}
}

func (r *IstioRevisionReconciler) mapOwnerToReconcileRequest(ctx context.Context, obj client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)
	ownerKind := v1alpha1.IstioRevisionKind
	ownerAPIGroup := v1alpha1.GroupVersion.Group

	var requests []reconcile.Request

	for _, ref := range obj.GetOwnerReferences() {
		refGV, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			log.Error(err, "Could not parse OwnerReference APIVersion", "api version", ref.APIVersion)
			continue
		}

		if ref.Kind == ownerKind && refGV.Group == ownerAPIGroup {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: ref.Name}})
		}
	}

	annotations := obj.GetAnnotations()
	namespacedName, kind, apiGroup := helm.GetOwnerFromAnnotations(annotations)
	if namespacedName != nil && kind == ownerKind && apiGroup == ownerAPIGroup {
		requests = append(requests, reconcile.Request{NamespacedName: *namespacedName})
	}

	// HACK: because CNI components are shared between multiple IstioRevisions, we need to trigger a reconcile
	// of all IstioRevisions that have CNI enabled whenever a CNI component changes so that their status is
	// updated (e.g. readiness).
	if obj.GetNamespace() == r.CNINamespace &&
		annotations != nil && annotations["meta.helm.sh/release-name"] == cniReleaseNameBase+"-cni" {

		revList := v1alpha1.IstioRevisionList{}
		if err := r.Client.List(ctx, &revList); err != nil {
			log.Error(err, "Could not list IstioRevisions")
		} else {
			for _, item := range revList.Items {
				if isCNIEnabled(item.Spec.Values) {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      item.Name,
							Namespace: item.Spec.Namespace,
						},
					})
				}
			}
		}
	}

	return requests
}

func (r *IstioRevisionReconciler) mapNamespaceToReconcileRequest(ctx context.Context, ns client.Object) []reconcile.Request {
	revision := getReferencedRevisionFromNamespace(ns.GetLabels())
	if revision != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: revision}}}
	}
	return nil
}

func (r *IstioRevisionReconciler) mapPodToReconcileRequest(ctx context.Context, pod client.Object) []reconcile.Request {
	// TODO: rewrite getReferencedRevisionFromPod to use lazy loading to avoid loading the namespace if the pod references a revision directly
	ns := corev1.Namespace{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: pod.GetNamespace()}, &ns)
	if err != nil {
		return nil
	}

	revision := getReferencedRevisionFromPod(pod.GetLabels(), pod.GetAnnotations(), ns.GetLabels())
	if revision != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: revision}}}
	}
	return nil
}

type validatingWebhookConfigPredicate struct {
	predicate.Funcs
}

func (v validatingWebhookConfigPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}

	if matched, _ := regexp.MatchString("istiod-.*-validator|istio-validator.*", e.ObjectNew.GetName()); matched {
		// Istiod updates the caBundle and failurePolicy fields in istiod-<ns>-validator and istio-validator[-<rev>]-<ns>
		// webhook configs. We must ignore changes to these fields to prevent an endless update loop.
		clearIgnoredFields(e.ObjectOld)
		clearIgnoredFields(e.ObjectNew)
		return !reflect.DeepEqual(e.ObjectNew, e.ObjectOld)
	}
	return true
}

func clearIgnoredFields(obj client.Object) {
	obj.SetResourceVersion("")
	obj.SetGeneration(0)
	obj.SetManagedFields(nil)
	if webhookConfig, ok := obj.(*admissionv1.ValidatingWebhookConfiguration); ok {
		for i := 0; i < len(webhookConfig.Webhooks); i++ {
			webhookConfig.Webhooks[i].FailurePolicy = nil
		}
	}
}
