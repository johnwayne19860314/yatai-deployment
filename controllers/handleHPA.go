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

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/jinzhu/copier"
	"github.com/pkg/errors"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	autoscalingv2beta2 "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonconsts "github.com/bentoml/yatai-common/consts"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"

	servingv2alpha1 "github.com/bentoml/yatai-deployment/apis/serving/v2alpha1"
)

func (r *BentoDeploymentReconciler) createOrUpdateHPA(ctx context.Context, bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string) (modified bool, err error) {
	logs := log.FromContext(ctx)

	hpa, err := r.generateHPA(bentoDeployment, bento, runnerName)
	if err != nil {
		return
	}
	logs = logs.WithValues("namespace", hpa.Namespace, "hpaName", hpa.Name)
	hpaNamespacedName := fmt.Sprintf("%s/%s", hpa.Namespace, hpa.Name)

	legacyHPA, err := r.handleLegacyHPA(hpa)
	if err != nil {
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "HandleHPA", "Failed to convert HPA version %s: %s", hpaNamespacedName, err)
		logs.Error(err, "Failed to convert HPA.")
		return
	}

	r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetHPA", "Getting HPA %s", hpaNamespacedName)

	oldHPA, err := r.getHPA(ctx, hpa)
	oldHPAIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !oldHPAIsNotFound {
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "GetHPA", "Failed to get HPA %s: %s", hpaNamespacedName, err)
		logs.Error(err, "Failed to get HPA.")
		return
	}

	if oldHPAIsNotFound {
		logs.Info("HPA not found. Creating a new one.")

		err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(legacyHPA), "set last applied annotation for hpa %s", hpa.Name)
		if err != nil {
			logs.Error(err, "Failed to set last applied annotation.")
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for HPA %s: %s", hpaNamespacedName, err)
			return
		}

		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "CreateHPA", "Creating a new HPA %s", hpaNamespacedName)
		err = r.Create(ctx, legacyHPA)
		if err != nil {
			logs.Error(err, "Failed to create HPA.")
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "CreateHPA", "Failed to create HPA %s: %s", hpaNamespacedName, err)
			return
		}
		logs.Info("HPA created.")
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "CreateHPA", "Created HPA %s", hpaNamespacedName)
		modified = true
	} else {
		logs.Info("HPA found.")

		var patchResult *patch.PatchResult
		patchResult, err = patch.DefaultPatchMaker.Calculate(oldHPA, legacyHPA)
		if err != nil {
			logs.Error(err, "Failed to calculate patch.")
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "CalculatePatch", "Failed to calculate patch for HPA %s: %s", hpaNamespacedName, err)
			return
		}

		if !patchResult.IsEmpty() {
			logs.Info(fmt.Sprintf("HPA spec is different. Updating HPA. The patch result is: %s", patchResult.String()))

			err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(legacyHPA), "set last applied annotation for hpa %s", hpa.Name)
			if err != nil {
				logs.Error(err, "Failed to set last applied annotation.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for HPA %s: %s", hpaNamespacedName, err)
				return
			}

			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateHPA", "Updating HPA %s", hpaNamespacedName)
			err = r.Update(ctx, legacyHPA)
			if err != nil {
				logs.Error(err, "Failed to update HPA.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "UpdateHPA", "Failed to update HPA %s: %s", hpaNamespacedName, err)
				return
			}
			logs.Info("HPA updated.")
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateHPA", "Updated HPA %s", hpaNamespacedName)
			modified = true
		} else {
			logs.Info("HPA spec is the same. Skipping update.")
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateHPA", "Skipping update HPA %s", hpaNamespacedName)
		}
	}

	return
}

func (r *BentoDeploymentReconciler) generateHPA(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string) (*autoscalingv2beta2.HorizontalPodAutoscaler, error) {
	labels := r.getKubeLabels(bentoDeployment, bento, runnerName)

	annotations := r.getKubeAnnotations(bentoDeployment, bento, runnerName)

	kubeName := r.getKubeName(bentoDeployment, bento, runnerName, false)

	kubeNs := bentoDeployment.Namespace

	var hpaConf *servingv2alpha1.Autoscaling

	if runnerName != nil {
		for _, runner := range bentoDeployment.Spec.Runners {
			if runner.Name == *runnerName {
				hpaConf = runner.Autoscaling
				break
			}
		}
	} else {
		hpaConf = bentoDeployment.Spec.Autoscaling
	}

	if hpaConf == nil {
		hpaConf = &servingv2alpha1.Autoscaling{
			MinReplicas: 1,
			MaxReplicas: 1,
		}
	}

	kubeHpa := &autoscalingv2beta2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        kubeName,
			Namespace:   kubeNs,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: autoscalingv2beta2.HorizontalPodAutoscalerSpec{
			MinReplicas: &hpaConf.MinReplicas,
			MaxReplicas: hpaConf.MaxReplicas,
			ScaleTargetRef: autoscalingv2beta2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       kubeName,
			},
			Metrics: hpaConf.Metrics,
		},
	}

	if len(kubeHpa.Spec.Metrics) == 0 {
		averageUtilization := int32(commonconsts.HPACPUDefaultAverageUtilization)
		kubeHpa.Spec.Metrics = []autoscalingv2beta2.MetricSpec{
			{
				Type: autoscalingv2beta2.ResourceMetricSourceType,
				Resource: &autoscalingv2beta2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2beta2.MetricTarget{
						Type:               autoscalingv2beta2.UtilizationMetricType,
						AverageUtilization: &averageUtilization,
					},
				},
			},
		}
	}

	err := ctrl.SetControllerReference(bentoDeployment, kubeHpa, r.Scheme)
	if err != nil {
		return nil, errors.Wrapf(err, "set hpa %s controller reference", kubeName)
	}

	return kubeHpa, err
}

func (r *BentoDeploymentReconciler) getHPA(ctx context.Context, hpa *autoscalingv2beta2.HorizontalPodAutoscaler) (client.Object, error) {
	name, ns := hpa.Name, hpa.Namespace
	if r.requireLegacyHPA() {
		obj := &autoscalingv2beta2.HorizontalPodAutoscaler{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)
		if err == nil {
			obj.Status = hpa.Status
		}
		return obj, err
	}
	obj := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj)
	if err == nil {
		legacyStatus := &autoscalingv2.HorizontalPodAutoscalerStatus{}
		if err := copier.Copy(legacyStatus, obj.Status); err != nil {
			return nil, err
		}
		obj.Status = *legacyStatus
	}
	return obj, err
}

func (r *BentoDeploymentReconciler) handleLegacyHPA(hpa *autoscalingv2beta2.HorizontalPodAutoscaler) (client.Object, error) {
	if r.requireLegacyHPA() {
		return hpa, nil
	}
	v2hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := copier.Copy(v2hpa, hpa); err != nil {
		return nil, err
	}
	return v2hpa, nil
}

func (r *BentoDeploymentReconciler) requireLegacyHPA() bool {
	if r.ServerVersion.Major >= legacyHPAMajorVersion && r.ServerVersion.Minor > legacyHPAMinorVersion {
		return false
	}
	return true
}
