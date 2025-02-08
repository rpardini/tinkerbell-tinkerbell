/*
Copyright 2022 Tinkerbell.

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

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/tinkerbell/tinkerbell/api/bmc/v1alpha1"
)

// Index key for Job Owner Name.
const jobOwnerKey = ".metadata.controller"

// JobReconciler reconciles a Job object.
type JobReconciler struct {
	client client.Client
}

// NewJobReconciler returns a new JobReconciler.
func NewJobReconciler(c client.Client) *JobReconciler {
	return &JobReconciler{
		client: c,
	}
}

//+kubebuilder:rbac:groups=bmc.tinkerbell.org,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=bmc.tinkerbell.org,resources=jobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=bmc.tinkerbell.org,resources=jobs/finalizers,verbs=update

// Reconcile runs a Job.
// Creates the individual Tasks on the cluster.
// Watches for Task and creates next Job Task based on conditions.
func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("controllers/Job").WithValues("job", req.NamespacedName)
	logger.Info("Reconciling Job")

	// Fetch the job object
	job := &v1alpha1.Job{}
	err := r.client.Get(ctx, req.NamespacedName, job)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get Job")
		return ctrl.Result{}, err
	}

	// Deletion is a noop.
	if !job.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Job is Completed or Failed is noop.
	if job.HasCondition(v1alpha1.JobCompleted, v1alpha1.ConditionTrue) ||
		job.HasCondition(v1alpha1.JobFailed, v1alpha1.ConditionTrue) {
		return ctrl.Result{}, nil
	}

	// Create a patch from the initial Job object
	// Patch is used to update Status after reconciliation
	jobPatch := client.MergeFrom(job.DeepCopy())

	return r.doReconcile(ctx, job, jobPatch)
}

func (r *JobReconciler) doReconcile(ctx context.Context, job *v1alpha1.Job, jobPatch client.Patch) (ctrl.Result, error) {
	// Check if Job is not currently Running
	// Initialize the StartTime for the Job
	// Set the Job to Running condition True
	if !job.HasCondition(v1alpha1.JobRunning, v1alpha1.ConditionTrue) {
		now := metav1.Now()
		job.Status.StartTime = &now
		job.SetCondition(v1alpha1.JobRunning, v1alpha1.ConditionTrue)
	}

	// Get Machine object for the Job
	// Requeue if error
	machine := &v1alpha1.Machine{}
	err := r.getMachine(ctx, job.Spec.MachineRef, machine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Job %s/%s MachineRef: %w", job.Namespace, job.Name, err)
	}

	// List all Task owned by Job
	tasks := &v1alpha1.TaskList{}
	err = r.client.List(ctx, tasks, client.MatchingFields{jobOwnerKey: job.Name}, client.InNamespace(job.Namespace))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list owned Tasks for Job %s/%s: %w", job.Namespace, job.Name, err)
	}

	completedTasksCount := 0
	// Iterate Task Items.
	// Count the number of completed tasks.
	// Set the Job condition Failed True if Task has failed.
	// If the Task has neither Completed or Failed is noop.
	for _, task := range tasks.Items {
		if task.HasCondition(v1alpha1.TaskCompleted, v1alpha1.ConditionTrue) {
			completedTasksCount++
			continue
		}

		if task.HasCondition(v1alpha1.TaskFailed, v1alpha1.ConditionTrue) {
			err := fmt.Errorf("task %s/%s failed", task.Namespace, task.Name)
			job.SetCondition(v1alpha1.JobFailed, v1alpha1.ConditionTrue, v1alpha1.WithJobConditionMessage(err.Error()))
			patchErr := r.patchStatus(ctx, job, jobPatch)
			if patchErr != nil {
				return ctrl.Result{}, utilerrors.NewAggregate([]error{patchErr, err})
			}

			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Check if all Job tasks have Completed
	// Set the Task CompletionTime
	// Set Task Condition Completed True
	if completedTasksCount == len(job.Spec.Tasks) {
		job.SetCondition(v1alpha1.JobCompleted, v1alpha1.ConditionTrue)
		now := metav1.Now()
		job.Status.CompletionTime = &now
		err = r.patchStatus(ctx, job, jobPatch)
		return ctrl.Result{}, err
	}

	// Create the first Task for the Job
	if err := r.createTaskWithOwner(ctx, *job, completedTasksCount, machine.Spec.Connection); err != nil {
		// Set the Job condition Failed True
		job.SetCondition(v1alpha1.JobFailed, v1alpha1.ConditionTrue, v1alpha1.WithJobConditionMessage(err.Error()))
		patchErr := r.patchStatus(ctx, job, jobPatch)
		if patchErr != nil {
			return ctrl.Result{}, utilerrors.NewAggregate([]error{patchErr, err})
		}

		return ctrl.Result{}, err
	}

	// Patch the status at the end of reconcile loop
	err = r.patchStatus(ctx, job, jobPatch)
	return ctrl.Result{}, err
}

// getMachine Gets the Machine from MachineRef.
func (r *JobReconciler) getMachine(ctx context.Context, reference v1alpha1.MachineRef, machine *v1alpha1.Machine) error {
	key := types.NamespacedName{Namespace: reference.Namespace, Name: reference.Name}
	err := r.client.Get(ctx, key, machine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("machine %s not found: %w", key, err)
		}
		return fmt.Errorf("failed to get Machine %s: %w", key, err)
	}

	return nil
}

// createTaskWithOwner creates a Task object with an OwnerReference set to the Job.
func (r *JobReconciler) createTaskWithOwner(ctx context.Context, job v1alpha1.Job, taskIndex int, conn v1alpha1.Connection) error {
	isController := true
	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.FormatTaskName(job, taskIndex),
			Namespace: job.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: job.APIVersion,
					Kind:       job.Kind,
					Name:       job.Name,
					UID:        job.ObjectMeta.UID,
					Controller: &isController,
				},
			},
		},
		Spec: v1alpha1.TaskSpec{
			Task:       job.Spec.Tasks[taskIndex],
			Connection: conn,
		},
	}

	err := r.client.Create(ctx, task)
	if err != nil {
		return fmt.Errorf("failed to create Task %s/%s: %w", task.Namespace, task.Name, err)
	}

	return nil
}

// patchStatus patches the specified patch on the Job.
func (r *JobReconciler) patchStatus(ctx context.Context, job *v1alpha1.Job, patch client.Patch) error {
	err := r.client.Status().Patch(ctx, job, patch)
	if err != nil {
		return fmt.Errorf("failed to patch Job %s/%s status: %w", job.Namespace, job.Name, err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JobReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&v1alpha1.Task{},
		jobOwnerKey,
		TaskOwnerIndexFunc,
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Job{}).
		Watches(
			&v1alpha1.Task{},
			handler.EnqueueRequestForOwner(
				mgr.GetScheme(),
				mgr.GetRESTMapper(),
				&v1alpha1.Job{},
				handler.OnlyControllerOwner(),
			),
		).
		Complete(r)
}

// TaskOwnerIndexFunc is Indexer func which returns the owner name for obj.
func TaskOwnerIndexFunc(obj client.Object) []string {
	task, ok := obj.(*v1alpha1.Task)
	if !ok {
		return nil
	}

	owner := metav1.GetControllerOf(task)
	if owner == nil {
		return nil
	}

	// Check if owner is Job
	if owner.Kind != "Job" || owner.APIVersion != v1alpha1.GroupVersion.String() {
		return nil
	}

	return []string{owner.Name}
}
