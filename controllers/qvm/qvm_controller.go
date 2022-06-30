/*
.
*/

package qvm

import (
	"context"

	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/util/workqueue"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	qvmv1alpha1 "qiniu.com/qvirt/apis/qvm/v1alpha1"
)

const AnnotQVMVMI = "qvm.qiniu.com/vmi"

// QvmReconciler reconciles a Qvm object
type QvmReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=qvm.qiniu.com,resources=qvms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=qvm.qiniu.com,resources=qvms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=qvm.qiniu.com,resources=qvms/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Qvm object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *QvmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	var qvm qvmv1alpha1.Qvm
	err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, &qvm)
	if err != nil {
		return ctrl.Result{}, err
	}

	var vm virtv1.VirtualMachine
	var vmi virtv1.VirtualMachineInstance
	err = r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, &vm)
	if err != nil {
		if apierrs.IsNotFound(err) {
			return r.createVM(ctx, &qvm)
		}
		return ctrl.Result{}, err
	}
	if err = r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, &vmi); err != nil {
		return ctrl.Result{}, err
	}
	return r.updateStatus(ctx, &qvm, &vm, &vmi)
}

func (r *QvmReconciler) createVM(ctx context.Context, qvm *qvmv1alpha1.Qvm) (ctrl.Result, error) {
	var vm virtv1.VirtualMachine
	vm.Spec = qvm.Spec.VM
	vm.Name = qvm.Name
	vm.Namespace = qvm.Namespace
	vm.Labels = qvm.Labels
	vm.Annotations = qvm.Annotations

	err := controllerutil.SetOwnerReference(qvm, &vm, r.Scheme)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(qvm.Spec.FloatingIPs) > 0 {
		sips, err := json.Marshal(qvm.Spec.FloatingIPs)
		if err != nil {
			return ctrl.Result{}, err
		}
		if vm.Spec.Template.ObjectMeta.Annotations == nil {
			vm.Spec.Template.ObjectMeta.Annotations = map[string]string{}
		}
		vm.Spec.Template.ObjectMeta.Annotations["cni.projectcalico.org/floatingIPs"] = string(sips)
		vm.Spec.Template.ObjectMeta.Annotations[AnnotQVMVMI] = "true"
	}
	return ctrl.Result{}, r.Create(ctx, &vm)
}

func (r *QvmReconciler) updateStatus(ctx context.Context, qvm *qvmv1alpha1.Qvm, vm *virtv1.VirtualMachine, vmi *virtv1.VirtualMachineInstance) (ctrl.Result, error) {
	qvm.Status.Phase = string(vmi.Status.Phase)
	qvm.Status.NodeName = vmi.Status.NodeName
	lg := log.FromContext(ctx)
	lg.Info("update status", "phase", qvm.Status.Phase, "nodeName", qvm.Status.NodeName)
	return ctrl.Result{}, r.Status().Update(ctx, qvm)
}

// SetupWithManager sets up the controller with the Manager.
func (r *QvmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&qvmv1alpha1.Qvm{}).
		Owns(&virtv1.VirtualMachine{}).
		Watches(&source.Kind{Type: &virtv1.VirtualMachineInstance{}}, &handler.Funcs{
			UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
				_ = (e.ObjectOld).(*virtv1.VirtualMachineInstance)
				now := (e.ObjectNew).(*virtv1.VirtualMachineInstance)
				if now.Annotations != nil && now.Annotations[AnnotQVMVMI] != "" {
					q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: now.Namespace, Name: now.Name}})
				}
			},
		}).
		Complete(r)
}
