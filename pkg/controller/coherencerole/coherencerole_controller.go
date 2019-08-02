// Package coherencerole contains the Coherence Operator controller for the CoherenceRole crd
package coherencerole

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	coh "github.com/oracle/coherence-operator/pkg/apis/coherence/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// The name of this controller. This is used in events, log messages, etc.
const (
	controllerName = "coherencerole-controller"

	invalidRoleEventMessage      string = "invalid CoherenceRole '%s' cannot find parent CoherenceCluster '%s'"
	createMessage                string = "created CoherenceInternal '%s' from CoherenceRole '%s' successful"
	createFailedMessage          string = "create CoherenceInternal '%s' from CoherenceRole '%s' failed\n%s"
	updateMessage                string = "updated CoherenceInternal %s from CoherenceRole %s successful"
	updateFailedMessage          string = "update CoherenceInternal %s from CoherenceRole %s failed\n%s"
	deleteMessage                string = "deleted CoherenceInternal %s from CoherenceRole %s successful"
	deleteFailedMessage          string = "delete CoherenceInternal %s from CoherenceRole %s failed\n%s"
	failedToGetHelmValuesMessage string = "Failed to get Helm values for CoherenceRole %s failed\n%s"
	failedToGetParentCluster     string = "Failed to get parent CoherenceCluster %s for CoherenceRole %s failed\n%s"
	failedToReconcileRole        string = "Failed to reconcile CoherenceRole %s due to error\n%s"

	eventReasonFailed       string = "failed"
	eventReasonCreated      string = "SuccessfulCreate"
	eventReasonFailedCreate string = "FailedCreate"
	eventReasonUpdated      string = "SuccessfulUpdate"
	eventReasonFailedUpdate string = "FailedUpdate"
	eventReasonDeleted      string = "SuccessfulDelete"
	eventReasonFailedDelete string = "FailedDelete"
	eventReasonScale        string = "Scaling"

	// The template used to create the CoherenceRole.Status.Selector
	selectorTemplate = "coherenceCluster=%s,coherenceRole=%s"
)

var log = logf.Log.WithName(controllerName)

// Add creates a new CoherenceRole Controller and adds it to the Manager. The Manager will set fields on the Controller.
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler.
func newReconciler(mgr manager.Manager) *ReconcileCoherenceRole {
	scheme := mgr.GetScheme()
	gvk := coh.GetCoherenceInternalGroupVersionKind(scheme)

	return &ReconcileCoherenceRole{
		client: mgr.GetClient(),
		scheme: scheme,
		gvk:    gvk,
		events: mgr.GetRecorder(controllerName),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler.
func add(mgr manager.Manager, r *ReconcileCoherenceRole) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource CoherenceRole
	err = c.Watch(&source.Kind{Type: &coh.CoherenceRole{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource - in this case we watch the StatefulSet created by the Helm chart
	err = c.Watch(&source.Kind{Type: &appsv1.StatefulSet{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    r.CreateEmptyHelmValues(nil),
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileCoherenceRole implements reconcile.Reconciler.
// If the reconcile.Reconciler API was to change then we'd get a compile error here.
var _ reconcile.Reconciler = &ReconcileCoherenceRole{}

// ReconcileCoherenceRole reconciles a CoherenceRole object and related CoherenceInternal
// Helm values resources.
type ReconcileCoherenceRole struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the api server
	client client.Client
	scheme *runtime.Scheme
	gvk    schema.GroupVersionKind
	events record.EventRecorder
}

// Reconcile reads that state of a CoherenceRole object and makes changes based on the state read
// and what is in the CoherenceRole.Spec.
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileCoherenceRole) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	logger := log.WithValues("Namespace", request.Namespace, "Name", request.Name)
	logger.Info("Reconciling CoherenceRole")

	// Fetch the CoherenceRole role
	role := &coh.CoherenceRole{}
	err := r.client.Get(context.TODO(), request.NamespacedName, role)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			logger.Info("CoherenceRole not found - assuming normal deletion")
			return reconcile.Result{Requeue: false}, nil
		}
		// Error reading the object - requeue the request.
		// We can't call the error handler as we do not even have a role.
		// We log the error and do not requeue the request.
		logger.Error(err, "Error getting CoherenceRole to reconcile")
		return r.handleErrAndRequeue(err, nil, fmt.Sprintf(failedToReconcileRole, role.Name, err), logger)
	}

	clusterName := role.GetCoherenceClusterName()

	// Fetch the owning CoherenceCluster
	cluster := &coh.CoherenceCluster{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: request.Namespace, Name: clusterName}, cluster)
	if err != nil {
		if errors.IsNotFound(err) {
			return r.handleErrAndFinish(nil, role, fmt.Sprintf(invalidRoleEventMessage, role.Name, clusterName), logger)
		} else {
			return r.handleErrAndRequeue(err, role, fmt.Sprintf(failedToGetParentCluster, clusterName, role.Name, err.Error()), logger)
		}
	}

	// find the existing Helm values structure in k8s (this will be an unstructured.Unstructured)
	// it may not exist if this is a create request
	helmValues, err := r.GetExistingHelmValues(role)

	if err != nil {
		if errors.IsNotFound(err) {
			// Helm values was not found so this is an insert of a new role
			return r.createRole(cluster, role)
		} else {
			// the error is a real error
			return r.handleErrAndRequeue(err, role, fmt.Sprintf(failedToGetHelmValuesMessage, role.Name, err.Error()), logger)
		}
	} else {
		// The Helm values was found so this is an update
		return r.updateRole(cluster, role, helmValues)
	}
}

// createRole creates a new Helm values structure in k8s, which will in turn trigger a Helm install.
func (r *ReconcileCoherenceRole) createRole(cluster *coh.CoherenceCluster, role *coh.CoherenceRole) (reconcile.Result, error) {
	if role.Spec.GetReplicas() <= 0 {
		// nothing to do as the desired replica count is zero
		return reconcile.Result{}, nil
	}

	logger := log.WithValues("Namespace", role.Namespace, "Name", role.Name)
	logger.Info("Creating Coherence Role Helm values")

	// define a new Helm values map
	spec, err := coh.NewCoherenceInternalSpecAsMap(cluster, role)
	if err != nil {
		// this error would only occur if there was a json marshalling issue which would be unlikely
		return r.handleErrAndRequeue(err, nil, fmt.Sprintf(createFailedMessage, role.Name, role.Name, err), logger)
	}

	helmValues := r.CreateHelmValues(cluster, role, spec)

	// Set this CoherenceRole instance as the owner and controller of the Helm values structure
	if err := controllerutil.SetControllerReference(cluster, helmValues, r.scheme); err != nil {
		return r.handleErrAndRequeue(err, nil, fmt.Sprintf(createFailedMessage, helmValues.GetName(), role.Name, err), logger)
	}

	// Create the CoherenceInternal resource in k8s which will be detected
	// by the Helm operator and trigger a Helm install
	if err := r.client.Create(context.TODO(), helmValues); err != nil {
		return r.handleErrAndRequeue(err, nil, fmt.Sprintf(createFailedMessage, helmValues.GetName(), role.Name, err), logger)
	}

	// update this CoherenceRole's status
	role.Status.Status = coh.RoleStatusCreated
	role.Status.Replicas = role.Spec.GetReplicas()
	role.Status.Selector = fmt.Sprintf(selectorTemplate, cluster.Name, role.Spec.GetRoleName())
	err = r.client.Status().Update(context.TODO(), role)
	if err != nil {
		// failed to update the CoherenceRole's status
		// ToDo - handle this properly by re-queuing the request and then in the reconcile method properly handle setting status even if the role is in the desired state
		logger.Error(err, "failed to update role status")
	}

	// send a successful creation event
	msg := fmt.Sprintf(createMessage, helmValues.GetName(), role.Name)
	r.events.Event(role, corev1.EventTypeNormal, eventReasonCreated, msg)

	return reconcile.Result{Requeue: false}, nil
}

// updateRole updates an existing CoherenceInternal which will in turn trigger a Helm update.
func (r *ReconcileCoherenceRole) updateRole(cluster *coh.CoherenceCluster, role *coh.CoherenceRole, helmValues *unstructured.Unstructured) (reconcile.Result, error) {
	logger := log.WithValues("Namespace", role.Namespace, "Name", role.Name)
	logger.Info("Reconciling existing Coherence Role")

	// ToDo: Fix this code to work when there is only a default role in the cluster spec and also how to work with the role + defaults
	//clusterRole := cluster.GetRole(role.Spec.GetRoleName())
	//if !reflect.DeepEqual(clusterRole, role.Spec) {
	//	// Role spec is not the same as the cluster's role spec - likely caused by a scale but could have
	//	// been caused by a direct update to the CoherenceRole, even though we really discourage that.
	//	// Update the cluster which will cause this update to come around again.
	//	logger.Info("CoherenceCluster role spec is different to this spec - updating CoherenceCluster '" + cluster.Name + "'")
	//	cluster.SetRole(role.Spec)
	//	err := r.client.Update(context.TODO(), cluster)
	//	if err != nil {
	//		return r.handleErrAndRequeue(err, nil, fmt.Sprintf(updateFailedMessage, helmValues.GetName(), role.Name, err), logger)
	//	}
	//}

	// convert the unstructured data to a CoherenceInternal that we can deal with better
	existing, err := r.toCoherenceInternal(helmValues)
	if err != nil {
		return r.handleErrAndRequeue(err, nil, fmt.Sprintf(updateFailedMessage, helmValues.GetName(), role.Name, err), logger)
	}

	currentReplicas := existing.Spec.ClusterSize
	desiredReplicas := role.Spec.GetReplicas()
	desiredRole := coh.NewCoherenceInternalSpec(cluster, role)
	clone := existing.Spec.DeepCopy()
	clone.ClusterSize = desiredRole.ClusterSize
	isUpgrade := !reflect.DeepEqual(&clone, desiredRole)

	sts, err := r.findStatefulSet(role)
	if err != nil {
		logger.Info("Could not find StatefulSet")
	}

	if currentReplicas < desiredReplicas {
		// Scaling UP

		// if scaling up and upgrading then upgrade first and scale second
		// otherwise we'd have to upgrade all the scaled up members
		if isUpgrade {
			result, err := r.upgrade(role, helmValues, currentReplicas, desiredRole)
			if err == nil {
				// requeue so that we then scale up after the upgrade
				return reconcile.Result{Requeue: true}, nil
			} else {
				return result, err
			}
		}

		logger.Info(fmt.Sprintf("Scaling up existing Role from %d to %d", currentReplicas, desiredReplicas))
		return r.scale(role, helmValues, existing, desiredReplicas, currentReplicas, sts)
	} else if currentReplicas > desiredReplicas {
		// Scaling DOWN

		// if scaling down and upgrading then scale down first and upgrade second
		// so that we do not have to upgrade the members we are scaling down
		logger.Info(fmt.Sprintf("Scaling down existing Role from %d to %d", currentReplicas, desiredReplicas))
		result, err := r.scale(role, helmValues, existing, desiredReplicas, currentReplicas, sts)

		if err == nil && isUpgrade {
			// requeue the request so that we then upgrade
			return reconcile.Result{Requeue: true}, nil
		} else {
			return result, err
		}
	} else if isUpgrade {
		// no scaling, just a rolling upgrade
		return r.upgrade(role, helmValues, currentReplicas, desiredRole)
	} else if sts != nil {
		// nothing to do to update or scale
		// We probably arrived here due to a change in the StatefulSet for a role
		// In this case we can potentially update the role's status based on what changed in the StatefulSet

		if role.Status.CurrentReplicas != sts.Status.Replicas || role.Status.ReadyReplicas != sts.Status.ReadyReplicas {
			// Update this CoherenceRole's status
			role.Status.CurrentReplicas = sts.Status.CurrentReplicas
			role.Status.ReadyReplicas = sts.Status.ReadyReplicas
			if sts.Status.ReadyReplicas == desiredReplicas {
				role.Status.Status = coh.RoleStatusReady
			}
			err = r.client.Status().Update(context.TODO(), role)
			if err != nil {
				// failed to update the CoherenceRole's status
				// ToDo - handle this properly by re-queuing the request and then in the reconcile method properly handle setting status even if the role is in the desired state
				log.Error(err, "failed to update role status", "Namespace", role.Namespace, "Name", role.Name)
			}
		}
	}

	return reconcile.Result{Requeue: false}, nil
}

// upgrade triggers a rolling upgrade of the role
func (r *ReconcileCoherenceRole) upgrade(role *coh.CoherenceRole, existingRole *unstructured.Unstructured, replicas int32, desiredRole *coh.CoherenceInternalSpec) (reconcile.Result, error) {
	// Rolling upgrade
	reqLogger := log.WithValues("Namespace", role.Namespace, "Name", role.Name)
	reqLogger.Info("Rolling upgrade of existing Role")

	spec, err := coh.CoherenceInternalSpecAsMapFromSpec(desiredRole)
	if err != nil {
		// ToDo - handle this properly
		return reconcile.Result{}, err
	}

	// update the CoherenceInternal, this should trigger an update of the Helm install
	desiredRole.ClusterSize = replicas
	existingRole.Object["spec"] = spec

	err = r.client.Update(context.TODO(), existingRole)
	if err != nil {
		// ToDo - handle this properly
		return reconcile.Result{}, err
	}

	// Update this CoherenceRole's status
	role.Status.Status = coh.RoleStatusRollingUpgrade
	err = r.client.Update(context.TODO(), role)
	if err != nil {
		reqLogger.Error(err, "failed to update Status")
	}

	// send a successful scale event
	msg := fmt.Sprintf(updateMessage, role.Name, role.Name)
	r.events.Event(role, corev1.EventTypeNormal, eventReasonUpdated, msg)

	return reconcile.Result{Requeue: false}, nil
}

// findStatefulSet finds the StatefulSet associated to the role.
func (r *ReconcileCoherenceRole) findStatefulSet(role *coh.CoherenceRole) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: role.Name, Namespace: role.Namespace}, sts)

	if err != nil {
		return nil, err
	}

	return sts, nil
}

// GetExistingHelmValues gets an existing unstructured Helm values from k8s for a given CoherenceRole
func (r *ReconcileCoherenceRole) GetExistingHelmValues(role *coh.CoherenceRole) (*unstructured.Unstructured, error) {
	cohInt := r.CreateEmptyHelmValues(role)
	err := r.client.Get(context.TODO(), types.NamespacedName{Namespace: role.Namespace, Name: role.Name}, cohInt)
	return cohInt, err
}

// CreateHelmValues creates an unstructured Helm values struct.
func (r *ReconcileCoherenceRole) CreateHelmValues(cluster *coh.CoherenceCluster, role *coh.CoherenceRole, spec map[string]interface{}) *unstructured.Unstructured {
	cohInternal := r.CreateEmptyHelmValues(role)
	cohInternal.Object["spec"] = spec

	// Set the labels for the CoherenceInternal
	labels := make(map[string]string)
	labels[coh.CoherenceClusterLabel] = cluster.Name
	labels[coh.CoherenceRoleLabel] = role.Spec.GetRoleName()
	cohInternal.SetLabels(labels)

	return cohInternal
}

// CreateEmptyHelmValues creates an empty (no Spec) unstructured Helm values.
func (r *ReconcileCoherenceRole) CreateEmptyHelmValues(role *coh.CoherenceRole) *unstructured.Unstructured {
	cohInternal := &unstructured.Unstructured{}

	cohInternal.SetGroupVersionKind(r.gvk)

	if role != nil {
		cohInternal.SetNamespace(role.Namespace)
		cohInternal.SetName(role.Name)
	}

	return cohInternal
}

// toCoherenceInternal converts an unstructured Helm values struct to a real CoherenceInternal struct.
func (r *ReconcileCoherenceRole) toCoherenceInternal(state *unstructured.Unstructured) (*coh.CoherenceInternal, error) {
	b, err := state.MarshalJSON()
	if err != nil {
		return nil, err
	}

	cohInternal := &coh.CoherenceInternal{}
	gvk := &schema.GroupVersionKind{
		Group:   r.gvk.Group,
		Kind:    r.gvk.Kind,
		Version: r.gvk.Version,
	}

	_, _, err = unstructured.UnstructuredJSONScheme.Decode(b, gvk, cohInternal)
	if err != nil {
		return nil, err
	}

	return cohInternal, nil
}

// failed is the common error handler
func (r *ReconcileCoherenceRole) handleErrAndRequeue(err error, role *coh.CoherenceRole, msg string, logger logr.Logger) (reconcile.Result, error) {
	return r.failed(err, role, msg, true, logger)
}

// failed is the common error handler
func (r *ReconcileCoherenceRole) handleErrAndFinish(err error, role *coh.CoherenceRole, msg string, logger logr.Logger) (reconcile.Result, error) {
	return r.failed(err, role, msg, false, logger)
}

// failed is the common error handler
func (r *ReconcileCoherenceRole) failed(err error, role *coh.CoherenceRole, msg string, requeue bool, logger logr.Logger) (reconcile.Result, error) {
	if err == nil {
		logger.V(0).Info(msg)
	} else {
		logger.Error(err, msg)
	}

	if role != nil {
		// update the status to failed.
		role.Status.Status = coh.RoleStatusFailed
		if e := r.client.Status().Update(context.TODO(), role); e != nil {
			// There isn't much we can do, we're already handling an error
			logger.Error(err, "failed to update role status")
		}

		// send a failure event
		r.events.Event(role, corev1.EventTypeNormal, eventReasonFailed, msg)
	}

	if requeue {
		return reconcile.Result{Requeue: true}, nil
	} else {
		return reconcile.Result{Requeue: false}, nil
	}
}