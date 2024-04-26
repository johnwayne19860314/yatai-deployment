/*
Copyright 2022.

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

package controllers

import (
	"context"
	// nolint: gosec

	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	autoscalingv2beta2 "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	commonconsts "github.com/bentoml/yatai-common/consts"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"

	servingconversion "github.com/bentoml/yatai-deployment/apis/serving/conversion"
	servingv2alpha1 "github.com/bentoml/yatai-deployment/apis/serving/v2alpha1"
	"github.com/bentoml/yatai-deployment/version"
)

// BentoDeploymentReconciler reconciles a BentoDeployment object
type BentoDeploymentReconciler struct {
	client.Client
	ServerVersion *k8sversion.Info
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
}

//+kubebuilder:rbac:groups=serving.yatai.ai,resources=bentodeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=serving.yatai.ai,resources=bentodeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=serving.yatai.ai,resources=bentodeployments/finalizers,verbs=update

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the BentoDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *BentoDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logs := log.FromContext(ctx)

	bentoDeployment := &servingv2alpha1.BentoDeployment{}
	err = r.Get(ctx, req.NamespacedName, bentoDeployment)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			logs.Info("BentoDeployment resource not found. Ignoring since object must be deleted.")
			err = nil
			return
		}
		// Error reading the object - requeue the request.
		logs.Error(err, "Failed to get BentoDeployment.")
		return
	}

	logs = logs.WithValues("bentoDeployment", bentoDeployment.Name, "namespace", bentoDeployment.Namespace)

	if bentoDeployment.Status.Conditions == nil || len(bentoDeployment.Status.Conditions) == 0 {
		logs.Info("Starting to reconcile BentoDeployment")
		logs.Info("Initializing BentoDeployment status")
		r.Recorder.Event(bentoDeployment, corev1.EventTypeNormal, "Reconciling", "Starting to reconcile BentoDeployment")
		bentoDeployment, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    servingv2alpha1.BentoDeploymentConditionTypeAvailable,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Starting to reconcile BentoDeployment",
			},
			metav1.Condition{
				Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoFound,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Starting to reconcile BentoDeployment",
			},
		)
		if err != nil {
			return
		}
	}

	defer func() {
		if err == nil {
			return
		}
		logs.Error(err, "Failed to reconcile BentoDeployment.")
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "ReconcileError", "Failed to reconcile BentoDeployment: %v", err)
		_, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    servingv2alpha1.BentoDeploymentConditionTypeAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Failed to reconcile BentoDeployment: %v", err),
			},
		)
		if err != nil {
			return
		}
	}()

	yataiClient, clusterName, err := r.getYataiClient(ctx)
	if err != nil {
		err = errors.Wrap(err, "get yatai client")
		return
	}

	bentoFoundCondition := meta.FindStatusCondition(bentoDeployment.Status.Conditions, servingv2alpha1.BentoDeploymentConditionTypeBentoFound)
	if bentoFoundCondition != nil && bentoFoundCondition.Status == metav1.ConditionUnknown {
		logs.Info(fmt.Sprintf("Getting Bento %s", bentoDeployment.Spec.Bento))
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetBento", "Getting Bento %s", bentoDeployment.Spec.Bento)
	}
	bentoRequest := &resourcesv1alpha1.BentoRequest{}
	bentoCR := &resourcesv1alpha1.Bento{}
	err = r.Get(ctx, types.NamespacedName{
		Namespace: bentoDeployment.Namespace,
		Name:      bentoDeployment.Spec.Bento,
	}, bentoCR)
	bentoIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !bentoIsNotFound {
		err = errors.Wrapf(err, "get Bento %s/%s", bentoDeployment.Namespace, bentoDeployment.Spec.Bento)
		return
	}
	if bentoIsNotFound {
		if bentoFoundCondition != nil && bentoFoundCondition.Status == metav1.ConditionUnknown {
			logs.Info(fmt.Sprintf("Bento %s not found", bentoDeployment.Spec.Bento))
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetBento", "Bento %s not found", bentoDeployment.Spec.Bento)
		}
		bentoDeployment, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoFound,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: "Bento not found",
			},
		)
		if err != nil {
			return
		}
		bentoRequestFoundCondition := meta.FindStatusCondition(bentoDeployment.Status.Conditions, servingv2alpha1.BentoDeploymentConditionTypeBentoRequestFound)
		if bentoRequestFoundCondition == nil || bentoRequestFoundCondition.Status != metav1.ConditionUnknown {
			bentoDeployment, err = r.setStatusConditions(ctx, req,
				metav1.Condition{
					Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoRequestFound,
					Status:  metav1.ConditionUnknown,
					Reason:  "Reconciling",
					Message: "Bento not found",
				},
			)
			if err != nil {
				return
			}
		}
		if bentoRequestFoundCondition != nil && bentoRequestFoundCondition.Status == metav1.ConditionUnknown {
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetBentoRequest", "Getting BentoRequest %s", bentoDeployment.Spec.Bento)
		}
		err = r.Get(ctx, types.NamespacedName{
			Namespace: bentoDeployment.Namespace,
			Name:      bentoDeployment.Spec.Bento,
		}, bentoRequest)
		if err != nil {
			err = errors.Wrapf(err, "get BentoRequest %s/%s", bentoDeployment.Namespace, bentoDeployment.Spec.Bento)
			bentoDeployment, err = r.setStatusConditions(ctx, req,
				metav1.Condition{
					Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoFound,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: err.Error(),
				},
				metav1.Condition{
					Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoRequestFound,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: err.Error(),
				},
			)
			if err != nil {
				return
			}
		}
		if bentoRequestFoundCondition != nil && bentoRequestFoundCondition.Status == metav1.ConditionUnknown {
			logs.Info(fmt.Sprintf("BentoRequest %s found", bentoDeployment.Spec.Bento))
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetBentoRequest", "BentoRequest %s is found and waiting for its bento to be provided", bentoDeployment.Spec.Bento)
		}
		bentoDeployment, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoRequestFound,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: "Bento not found",
			},
		)
		if err != nil {
			return
		}
		bentoRequestAvailableCondition := meta.FindStatusCondition(bentoRequest.Status.Conditions, resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable)
		if bentoRequestAvailableCondition != nil && bentoRequestAvailableCondition.Status == metav1.ConditionFalse {
			err = errors.Errorf("BentoRequest %s/%s is not available: %s", bentoRequest.Namespace, bentoRequest.Name, bentoRequestAvailableCondition.Message)
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "GetBentoRequest", err.Error())
			_, err_ := r.setStatusConditions(ctx, req,
				metav1.Condition{
					Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoFound,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: err.Error(),
				},
				metav1.Condition{
					Type:    servingv2alpha1.BentoDeploymentConditionTypeAvailable,
					Status:  metav1.ConditionFalse,
					Reason:  "Reconciling",
					Message: err.Error(),
				},
			)
			if err_ != nil {
				err = err_
				return
			}
			return
		}
		return
	} else {
		if bentoFoundCondition != nil && bentoFoundCondition.Status != metav1.ConditionTrue {
			logs.Info(fmt.Sprintf("Bento %s found", bentoDeployment.Spec.Bento))
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetBento", "Bento %s is found", bentoDeployment.Spec.Bento)
		}
		bentoDeployment, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    servingv2alpha1.BentoDeploymentConditionTypeBentoFound,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: "Bento found",
			},
		)
		if err != nil {
			return
		}
	}

	modified := false

	if bentoCR.Spec.Runners != nil {
		for _, runner := range bentoCR.Spec.Runners {
			var modified_ bool
			// create or update runner deployment
			modified_, err = r.createOrUpdateOrDeleteDeployments(ctx, createOrUpdateOrDeleteDeploymentsOption{
				yataiClient:     yataiClient,
				bentoDeployment: bentoDeployment,
				bento:           bentoCR,
				runnerName:      &runner.Name,
				clusterName:     clusterName,
			})
			if err != nil {
				return
			}

			if modified_ {
				modified = true
			}

			// create or update hpa
			modified_, err = r.createOrUpdateHPA(ctx, bentoDeployment, bentoCR, &runner.Name)
			if err != nil {
				return
			}

			if modified_ {
				modified = true
			}

			// create or update service
			modified_, err = r.createOrUpdateOrDeleteServices(ctx, createOrUpdateOrDeleteServicesOption{
				bentoDeployment: bentoDeployment,
				bento:           bentoCR,
				runnerName:      &runner.Name,
			})
			if err != nil {
				return
			}

			if modified_ {
				modified = true
			}
		}
	}

	// create or update api-server deployment
	modified_, err := r.createOrUpdateOrDeleteDeployments(ctx, createOrUpdateOrDeleteDeploymentsOption{
		yataiClient:     yataiClient,
		bentoDeployment: bentoDeployment,
		bento:           bentoCR,
		runnerName:      nil,
		clusterName:     clusterName,
	})
	if err != nil {
		return
	}

	if modified_ {
		modified = true
	}

	// create or update api-server hpa
	modified_, err = r.createOrUpdateHPA(ctx, bentoDeployment, bentoCR, nil)
	if err != nil {
		return
	}

	if modified_ {
		modified = true
	}

	// create or update api-server service
	modified_, err = r.createOrUpdateOrDeleteServices(ctx, createOrUpdateOrDeleteServicesOption{
		bentoDeployment: bentoDeployment,
		bento:           bentoCR,
		runnerName:      nil,
	})
	if err != nil {
		return
	}

	if modified_ {
		modified = true
	}

	// create or update api-server ingresses
	modified_, err = r.createOrUpdateIngresses(ctx, createOrUpdateIngressOption{
		yataiClient:     yataiClient,
		bentoDeployment: bentoDeployment,
		bento:           bentoCR,
	})
	if err != nil {
		return
	}

	if modified_ {
		modified = true
	}

	if yataiClient != nil && clusterName != nil {
		yataiClient_ := *yataiClient
		clusterName_ := *clusterName
		bentoRepositoryName, bentoVersion := getBentoRepositoryNameAndBentoVersion(bentoCR)
		_, err = yataiClient_.GetBento(ctx, bentoRepositoryName, bentoVersion)
		bentoIsNotFound := err != nil && strings.Contains(err.Error(), "not found")
		if err != nil && !bentoIsNotFound {
			return
		}
		if bentoIsNotFound {
			bentoDeployment, err = r.setStatusConditions(ctx, req,
				metav1.Condition{
					Type:    servingv2alpha1.BentoDeploymentConditionTypeAvailable,
					Status:  metav1.ConditionTrue,
					Reason:  "Reconciling",
					Message: "Remote bento from Yatai is not found",
				},
			)
			return
		}
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetYataiDeployment", "Fetching yatai deployment %s", bentoDeployment.Name)
		var oldYataiDeployment *schemasv1.DeploymentSchema
		oldYataiDeployment, err = yataiClient_.GetDeployment(ctx, clusterName_, bentoDeployment.Namespace, bentoDeployment.Name)
		isNotFound := err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
		if err != nil && !isNotFound {
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "GetYataiDeployment", "Failed to fetch yatai deployment %s: %s", bentoDeployment.Name, err)
			return
		}
		err = nil

		envs := make([]*modelschemas.LabelItemSchema, 0)

		specEnvs := bentoDeployment.Spec.Envs

		for _, env := range specEnvs {
			envs = append(envs, &modelschemas.LabelItemSchema{
				Key:   env.Name,
				Value: env.Value,
			})
		}

		runners := make(map[string]modelschemas.DeploymentTargetRunnerConfig, 0)
		for _, runner := range bentoDeployment.Spec.Runners {
			envs_ := make([]*modelschemas.LabelItemSchema, 0)
			if runner.Envs != nil {
				for _, env := range runner.Envs {
					env := env
					envs_ = append(envs_, &modelschemas.LabelItemSchema{
						Key:   env.Name,
						Value: env.Value,
					})
				}
			}
			var hpaConf *modelschemas.DeploymentTargetHPAConf
			hpaConf, err = TransformToOldHPA(runner.Autoscaling)
			if err != nil {
				return
			}
			runners[runner.Name] = modelschemas.DeploymentTargetRunnerConfig{
				Resources:                              servingconversion.ConvertToDeploymentTargetResources(runner.Resources),
				HPAConf:                                hpaConf,
				Envs:                                   &envs_,
				EnableStealingTrafficDebugMode:         &[]bool{checkIfIsStealingTrafficDebugModeEnabled(runner.Annotations)}[0],
				EnableDebugMode:                        &[]bool{checkIfIsDebugModeEnabled(runner.Annotations)}[0],
				EnableDebugPodReceiveProductionTraffic: &[]bool{checkIfIsDebugPodReceiveProductionTrafficEnabled(runner.Annotations)}[0],
				BentoDeploymentOverrides: &modelschemas.RunnerBentoDeploymentOverrides{
					ExtraPodMetadata: runner.ExtraPodMetadata,
					ExtraPodSpec:     runner.ExtraPodSpec,
				},
			}
		}

		var hpaConf *modelschemas.DeploymentTargetHPAConf
		hpaConf, err = TransformToOldHPA(bentoDeployment.Spec.Autoscaling)
		if err != nil {
			return
		}
		deploymentTargets := make([]*schemasv1.CreateDeploymentTargetSchema, 0, 1)
		deploymentTarget := &schemasv1.CreateDeploymentTargetSchema{
			DeploymentTargetTypeSchema: schemasv1.DeploymentTargetTypeSchema{
				Type: modelschemas.DeploymentTargetTypeStable,
			},
			BentoRepository: bentoRepositoryName,
			Bento:           bentoVersion,
			Config: &modelschemas.DeploymentTargetConfig{
				KubeResourceUid:                        string(bentoDeployment.UID),
				KubeResourceVersion:                    bentoDeployment.ResourceVersion,
				Resources:                              servingconversion.ConvertToDeploymentTargetResources(bentoDeployment.Spec.Resources),
				HPAConf:                                hpaConf,
				Envs:                                   &envs,
				Runners:                                runners,
				EnableIngress:                          &bentoDeployment.Spec.Ingress.Enabled,
				EnableStealingTrafficDebugMode:         &[]bool{checkIfIsStealingTrafficDebugModeEnabled(bentoDeployment.Spec.Annotations)}[0],
				EnableDebugMode:                        &[]bool{checkIfIsDebugModeEnabled(bentoDeployment.Spec.Annotations)}[0],
				EnableDebugPodReceiveProductionTraffic: &[]bool{checkIfIsDebugPodReceiveProductionTrafficEnabled(bentoDeployment.Spec.Annotations)}[0],
				BentoDeploymentOverrides: &modelschemas.ApiServerBentoDeploymentOverrides{
					MonitorExporter:  bentoDeployment.Spec.MonitorExporter,
					ExtraPodMetadata: bentoDeployment.Spec.ExtraPodMetadata,
					ExtraPodSpec:     bentoDeployment.Spec.ExtraPodSpec,
				},
				BentoRequestOverrides: &modelschemas.BentoRequestOverrides{
					ImageBuildTimeout:              bentoRequest.Spec.ImageBuildTimeout,
					ImageBuilderExtraPodSpec:       bentoRequest.Spec.ImageBuilderExtraPodSpec,
					ImageBuilderExtraPodMetadata:   bentoRequest.Spec.ImageBuilderExtraPodMetadata,
					ImageBuilderExtraContainerEnv:  bentoRequest.Spec.ImageBuilderExtraContainerEnv,
					ImageBuilderContainerResources: bentoRequest.Spec.ImageBuilderContainerResources,
					DockerConfigJSONSecretName:     bentoRequest.Spec.DockerConfigJSONSecretName,
					DownloaderContainerEnvFrom:     bentoRequest.Spec.DownloaderContainerEnvFrom,
				},
			},
		}
		deploymentTargets = append(deploymentTargets, deploymentTarget)
		updateSchema := &schemasv1.UpdateDeploymentSchema{
			Targets:     deploymentTargets,
			DoNotDeploy: true,
		}
		if isNotFound {
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "CreateYataiDeployment", "Creating yatai deployment %s", bentoDeployment.Name)
			_, err = yataiClient_.CreateDeployment(ctx, clusterName_, &schemasv1.CreateDeploymentSchema{
				Name:                   bentoDeployment.Name,
				KubeNamespace:          bentoDeployment.Namespace,
				UpdateDeploymentSchema: *updateSchema,
			})
			if err != nil {
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "CreateYataiDeployment", "Failed to create yatai deployment %s: %s", bentoDeployment.Name, err)
				return
			}
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "CreateYataiDeployment", "Created yatai deployment %s", bentoDeployment.Name)
		} else {
			noChange := false
			if oldYataiDeployment != nil && oldYataiDeployment.LatestRevision != nil && len(oldYataiDeployment.LatestRevision.Targets) > 0 {
				oldYataiDeployment.LatestRevision.Targets[0].Config.KubeResourceUid = updateSchema.Targets[0].Config.KubeResourceUid
				oldYataiDeployment.LatestRevision.Targets[0].Config.KubeResourceVersion = updateSchema.Targets[0].Config.KubeResourceVersion
				noChange = reflect.DeepEqual(oldYataiDeployment.LatestRevision.Targets[0].Config, updateSchema.Targets[0].Config)
			}
			if noChange {
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateYataiDeployment", "No change in yatai deployment %s, skipping", bentoDeployment.Name)
			} else {
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateYataiDeployment", "Updating yatai deployment %s", bentoDeployment.Name)
				_, err = yataiClient_.UpdateDeployment(ctx, clusterName_, bentoDeployment.Namespace, bentoDeployment.Name, updateSchema)
				if err != nil {
					r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "UpdateYataiDeployment", "Failed to update yatai deployment %s: %s", bentoDeployment.Name, err)
					return
				}
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateYataiDeployment", "Updated yatai deployment %s", bentoDeployment.Name)
			}
		}
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "SyncYataiDeploymentStatus", "Syncing yatai deployment %s status", bentoDeployment.Name)
		_, err = yataiClient_.SyncDeploymentStatus(ctx, clusterName_, bentoDeployment.Namespace, bentoDeployment.Name)
		if err != nil {
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "SyncYataiDeploymentStatus", "Failed to sync yatai deployment %s status: %s", bentoDeployment.Name, err)
			return
		}
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "SyncYataiDeploymentStatus", "Synced yatai deployment %s status", bentoDeployment.Name)
	}

	if !modified {
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateYataiDeployment", "No changes to yatai deployment %s", bentoDeployment.Name)
	}

	logs.Info("Finished reconciling.")
	r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "Update", "All resources updated!")
	bentoDeployment, err = r.setStatusConditions(ctx, req,
		metav1.Condition{
			Type:    servingv2alpha1.BentoDeploymentConditionTypeAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Reconciling",
			Message: "Reconciling",
		},
	)
	return
}

// SetupWithManager sets up the controller with the Manager.
func (r *BentoDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logs := log.Log.WithValues("func", "SetupWithManager")
	version.Print()

	if os.Getenv("DISABLE_CLEANUP_ABANDONED_RUNNER_SERVICES") != commonconsts.KubeLabelValueTrue {
		go r.cleanUpAbandonedRunnerServices()
	} else {
		logs.Info("cleanup abandoned runner services is disabled")
	}

	if os.Getenv("DISABLE_YATAI_COMPONENT_REGISTRATION") != commonconsts.KubeLabelValueTrue {
		go r.registerYataiComponent()
	} else {
		logs.Info("yatai component registration is disabled")
	}

	m := ctrl.NewControllerManagedBy(mgr).
		For(&servingv2alpha1.BentoDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Service{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&networkingv1.Ingress{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&source.Kind{Type: &resourcesv1alpha1.BentoRequest{}}, handler.EnqueueRequestsFromMapFunc(func(bentoRequest client.Object) []reconcile.Request {
			reqs := make([]reconcile.Request, 0)
			logs := log.Log.WithValues("func", "Watches", "kind", "BentoRequest", "name", bentoRequest.GetName(), "namespace", bentoRequest.GetNamespace())
			bento := &resourcesv1alpha1.Bento{}
			err := r.Get(context.Background(), types.NamespacedName{
				Name:      bentoRequest.GetName(),
				Namespace: bentoRequest.GetNamespace(),
			}, bento)
			bentoIsNotFound := k8serrors.IsNotFound(err)
			if err != nil && !bentoIsNotFound {
				logs.Info("Failed to get bento", "name", bentoRequest.GetName(), "namespace", bentoRequest.GetNamespace())
				return reqs
			}
			if !bentoIsNotFound {
				return reqs
			}
			bentoDeployments := &servingv2alpha1.BentoDeploymentList{}
			err = r.List(context.Background(), bentoDeployments, &client.ListOptions{
				Namespace: bentoRequest.GetNamespace(),
			})
			if err != nil {
				logs.Info("Failed to list bentoDeployments")
				return reqs
			}
			for _, bentoDeployment := range bentoDeployments.Items {
				bentoDeployment := bentoDeployment
				if bentoDeployment.Spec.Bento == bentoRequest.GetName() {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&bentoDeployment),
					})
				}
			}
			return reqs
		})).
		Watches(&source.Kind{Type: &resourcesv1alpha1.Bento{}}, handler.EnqueueRequestsFromMapFunc(func(bento client.Object) []reconcile.Request {
			logs := log.Log.WithValues("func", "Watches", "kind", "Bento", "name", bento.GetName(), "namespace", bento.GetNamespace())
			bentoDeployments := &servingv2alpha1.BentoDeploymentList{}
			err := r.List(context.Background(), bentoDeployments, &client.ListOptions{
				Namespace: bento.GetNamespace(),
			})
			if err != nil {
				logs.Info("Failed to list bentoDeployments")
			}
			reqs := make([]reconcile.Request, 0)
			for _, bentoDeployment := range bentoDeployments.Items {
				bentoDeployment := bentoDeployment
				if bentoDeployment.Spec.Bento == bento.GetName() {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&bentoDeployment),
					})
				}
			}
			return reqs
		}))

	if r.requireLegacyHPA() {
		m.Owns(&autoscalingv2beta2.HorizontalPodAutoscaler{})
	} else {
		m.Owns(&autoscalingv2.HorizontalPodAutoscaler{})
	}
	return m.Complete(r)
}
