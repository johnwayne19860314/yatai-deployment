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
	"time"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"

	servingv2alpha1 "github.com/bentoml/yatai-deployment/apis/serving/v2alpha1"
)

func (r *BentoDeploymentReconciler) createOrUpdateOrDeleteServices(ctx context.Context, opt createOrUpdateOrDeleteServicesOption) (modified bool, err error) {
	resourceAnnotations := getResourceAnnotations(opt.bentoDeployment, opt.runnerName)
	isStealingTrafficDebugEnabled := checkIfIsStealingTrafficDebugModeEnabled(resourceAnnotations)
	isDebugPodReceiveProductionTrafficEnabled := checkIfIsDebugPodReceiveProductionTrafficEnabled(resourceAnnotations)
	containsStealingTrafficDebugModeEnabled := checkIfContainsStealingTrafficDebugModeEnabled(opt.bentoDeployment)
	modified, err = r.createOrUpdateService(ctx, createOrUpdateServiceOption{
		bentoDeployment:                         opt.bentoDeployment,
		bento:                                   opt.bento,
		runnerName:                              opt.runnerName,
		isStealingTrafficDebugModeEnabled:       false,
		isDebugPodReceiveProductionTraffic:      isDebugPodReceiveProductionTrafficEnabled,
		containsStealingTrafficDebugModeEnabled: containsStealingTrafficDebugModeEnabled,
		isGenericService:                        true,
	})
	if err != nil {
		return
	}
	if (opt.runnerName == nil && containsStealingTrafficDebugModeEnabled) || (opt.runnerName != nil && isStealingTrafficDebugEnabled) {
		var modified_ bool
		modified_, err = r.createOrUpdateService(ctx, createOrUpdateServiceOption{
			bentoDeployment:                         opt.bentoDeployment,
			bento:                                   opt.bento,
			runnerName:                              opt.runnerName,
			isStealingTrafficDebugModeEnabled:       false,
			isDebugPodReceiveProductionTraffic:      isDebugPodReceiveProductionTrafficEnabled,
			containsStealingTrafficDebugModeEnabled: containsStealingTrafficDebugModeEnabled,
			isGenericService:                        false,
		})
		if err != nil {
			return
		}
		if modified_ {
			modified = true
		}
		modified_, err = r.createOrUpdateService(ctx, createOrUpdateServiceOption{
			bentoDeployment:                         opt.bentoDeployment,
			bento:                                   opt.bento,
			runnerName:                              opt.runnerName,
			isStealingTrafficDebugModeEnabled:       true,
			isDebugPodReceiveProductionTraffic:      isDebugPodReceiveProductionTrafficEnabled,
			containsStealingTrafficDebugModeEnabled: containsStealingTrafficDebugModeEnabled,
			isGenericService:                        false,
		})
		if err != nil {
			return
		}
		if modified_ {
			modified = true
		}
	} else {
		productionServiceName := r.getServiceName(opt.bentoDeployment, opt.bento, opt.runnerName, false)
		svc := &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: productionServiceName, Namespace: opt.bentoDeployment.Namespace}, svc)
		isNotFound := k8serrors.IsNotFound(err)
		if err != nil && !isNotFound {
			err = errors.Wrapf(err, "Failed to get service %s", productionServiceName)
			return
		}
		if !isNotFound {
			modified = true
			err = r.Delete(ctx, svc)
			if err != nil {
				err = errors.Wrapf(err, "Failed to delete service %s", productionServiceName)
				return
			}
		}
		debugServiceName := r.getServiceName(opt.bentoDeployment, opt.bento, opt.runnerName, true)
		svc = &corev1.Service{}
		err = r.Get(ctx, types.NamespacedName{Name: debugServiceName, Namespace: opt.bentoDeployment.Namespace}, svc)
		isNotFound = k8serrors.IsNotFound(err)
		if err != nil && !isNotFound {
			err = errors.Wrapf(err, "Failed to get service %s", debugServiceName)
			return
		}
		err = nil
		if !isNotFound {
			modified = true
			err = r.Delete(ctx, svc)
			if err != nil {
				err = errors.Wrapf(err, "Failed to delete service %s", debugServiceName)
				return
			}
		}
	}
	return
}

func (r *BentoDeploymentReconciler) createOrUpdateService(ctx context.Context, opt createOrUpdateServiceOption) (modified bool, err error) {
	logs := log.FromContext(ctx)

	// nolint: gosimple
	service, err := r.generateService(generateServiceOption{
		bentoDeployment:                         opt.bentoDeployment,
		bento:                                   opt.bento,
		runnerName:                              opt.runnerName,
		isStealingTrafficDebugModeEnabled:       opt.isStealingTrafficDebugModeEnabled,
		isDebugPodReceiveProductionTraffic:      opt.isDebugPodReceiveProductionTraffic,
		containsStealingTrafficDebugModeEnabled: opt.containsStealingTrafficDebugModeEnabled,
		isGenericService:                        opt.isGenericService,
	})
	if err != nil {
		return
	}

	logs = logs.WithValues("namespace", service.Namespace, "serviceName", service.Name, "serviceSelector", service.Spec.Selector)

	serviceNamespacedName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)

	r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "GetService", "Getting Service %s", serviceNamespacedName)

	oldService := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, oldService)
	oldServiceIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !oldServiceIsNotFound {
		r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "GetService", "Failed to get Service %s: %s", serviceNamespacedName, err)
		logs.Error(err, "Failed to get Service.")
		return
	}

	if oldServiceIsNotFound {
		logs.Info("Service not found. Creating a new one.")

		err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(service), "set last applied annotation for service %s", service.Name)
		if err != nil {
			logs.Error(err, "Failed to set last applied annotation.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for Service %s: %s", serviceNamespacedName, err)
			return
		}

		r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "CreateService", "Creating a new Service %s", serviceNamespacedName)
		err = r.Create(ctx, service)
		if err != nil {
			logs.Error(err, "Failed to create Service.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "CreateService", "Failed to create Service %s: %s", serviceNamespacedName, err)
			return
		}
		logs.Info("Service created.")
		r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "CreateService", "Created Service %s", serviceNamespacedName)
		modified = true
	} else {
		logs.Info("Service found.")

		var patchResult *patch.PatchResult
		patchResult, err = patch.DefaultPatchMaker.Calculate(oldService, service)
		if err != nil {
			logs.Error(err, "Failed to calculate patch.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "CalculatePatch", "Failed to calculate patch for Service %s: %s", serviceNamespacedName, err)
			return
		}

		if !patchResult.IsEmpty() {
			logs.Info("Service spec is different. Updating Service.")

			err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(service), "set last applied annotation for service %s", service.Name)
			if err != nil {
				logs.Error(err, "Failed to set last applied annotation.")
				r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for Service %s: %s", serviceNamespacedName, err)
				return
			}

			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "UpdateService", "Updating Service %s", serviceNamespacedName)
			oldService.Annotations = service.Annotations
			oldService.Labels = service.Labels
			oldService.Spec = service.Spec
			err = r.Update(ctx, oldService)
			if err != nil {
				logs.Error(err, "Failed to update Service.")
				r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "UpdateService", "Failed to update Service %s: %s", serviceNamespacedName, err)
				return
			}
			logs.Info("Service updated.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "UpdateService", "Updated Service %s", serviceNamespacedName)
			modified = true
		} else {
			logs = logs.WithValues("oldServiceSelector", oldService.Spec.Selector)
			logs.Info("Service spec is the same. Skipping update.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "UpdateService", "Skipping update Service %s", serviceNamespacedName)
		}
	}

	return
}

func (r *BentoDeploymentReconciler) getRunnerServiceName(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName string, debug bool) string {
	bentoRepositoryName, bentoVersion := getBentoRepositoryNameAndBentoVersion(bento)
	hashStr := hash(fmt.Sprintf("%s:%s-%s", bentoRepositoryName, bentoVersion, runnerName))
	var svcName string
	if debug {
		svcName = fmt.Sprintf("%s-runner-d-%s", bentoDeployment.Name, hashStr)
	} else {
		svcName = fmt.Sprintf("%s-runner-p-%s", bentoDeployment.Name, hashStr)
	}
	if len(svcName) > 63 {
		if debug {
			svcName = fmt.Sprintf("runner-d-%s", hash(fmt.Sprintf("%s-%s:%s-%s", bentoDeployment.Name, bentoRepositoryName, bentoVersion, runnerName)))
		} else {
			svcName = fmt.Sprintf("runner-p-%s", hash(fmt.Sprintf("%s-%s:%s-%s", bentoDeployment.Name, bentoRepositoryName, bentoVersion, runnerName)))
		}
	}
	return svcName
}

func (r *BentoDeploymentReconciler) getRunnerGenericServiceName(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName string) string {
	bentoRepositoryName, bentoVersion := getBentoRepositoryNameAndBentoVersion(bento)
	hashStr := hash(fmt.Sprintf("%s:%s-%s", bentoRepositoryName, bentoVersion, runnerName))
	var svcName string
	svcName = fmt.Sprintf("%s-runner-%s", bentoDeployment.Name, hashStr)
	if len(svcName) > 63 {
		svcName = fmt.Sprintf("runner-%s", hash(fmt.Sprintf("%s-%s:%s-%s", bentoDeployment.Name, bentoRepositoryName, bentoVersion, runnerName)))
	}
	return svcName
}

func (r *BentoDeploymentReconciler) getServiceName(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string, debug bool) string {
	var kubeName string
	if runnerName != nil {
		kubeName = r.getRunnerServiceName(bentoDeployment, bento, *runnerName, debug)
	} else {
		if debug {
			kubeName = fmt.Sprintf("%s-d", bentoDeployment.Name)
		} else {
			kubeName = fmt.Sprintf("%s-p", bentoDeployment.Name)
		}
	}
	return kubeName
}

func (r *BentoDeploymentReconciler) getGenericServiceName(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string) string {
	var kubeName string
	if runnerName != nil {
		kubeName = r.getRunnerGenericServiceName(bentoDeployment, bento, *runnerName)
	} else {
		kubeName = r.getKubeName(bentoDeployment, bento, runnerName, false)
	}
	return kubeName
}

func (r *BentoDeploymentReconciler) generateService(opt generateServiceOption) (kubeService *corev1.Service, err error) {
	var kubeName string
	if opt.isGenericService {
		kubeName = r.getGenericServiceName(opt.bentoDeployment, opt.bento, opt.runnerName)
	} else {
		kubeName = r.getServiceName(opt.bentoDeployment, opt.bento, opt.runnerName, opt.isStealingTrafficDebugModeEnabled)
	}

	labels := r.getKubeLabels(opt.bentoDeployment, opt.bento, opt.runnerName)

	selector := make(map[string]string)

	for k, v := range labels {
		selector[k] = v
	}

	if opt.isStealingTrafficDebugModeEnabled {
		selector[commonconsts.KubeLabelYataiBentoDeploymentTargetType] = DeploymentTargetTypeDebug
	}

	targetPort := intstr.FromString(commonconsts.BentoContainerPortName)
	if opt.runnerName == nil {
		if opt.isGenericService {
			delete(selector, commonconsts.KubeLabelYataiBentoDeploymentTargetType)
			if opt.containsStealingTrafficDebugModeEnabled {
				targetPort = intstr.FromString(ContainerPortNameHTTPProxy)
			}
		}
	} else {
		if opt.isGenericService && opt.isDebugPodReceiveProductionTraffic {
			delete(selector, commonconsts.KubeLabelYataiBentoDeploymentTargetType)
		}
	}

	spec := corev1.ServiceSpec{
		Selector: selector,
		Ports: []corev1.ServicePort{
			{
				Name:       commonconsts.BentoServicePortName,
				Port:       commonconsts.BentoServicePort,
				TargetPort: targetPort,
				Protocol:   corev1.ProtocolTCP,
			},
			{
				Name:       ServicePortNameHTTPNonProxy,
				Port:       int32(ServicePortHTTPNonProxy),
				TargetPort: intstr.FromString(commonconsts.BentoContainerPortName),
				Protocol:   corev1.ProtocolTCP,
			},
		},
	}

	annotations := r.getKubeAnnotations(opt.bentoDeployment, opt.bento, opt.runnerName)

	kubeNs := opt.bentoDeployment.Namespace

	kubeService = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        kubeName,
			Namespace:   kubeNs,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: spec,
	}

	err = ctrl.SetControllerReference(opt.bentoDeployment, kubeService, r.Scheme)
	if err != nil {
		err = errors.Wrapf(err, "set controller reference for service %s", kubeService.Name)
		return
	}

	return
}

func (r *BentoDeploymentReconciler) doCleanUpAbandonedRunnerServices() error {
	logs := log.Log.WithValues("func", "doCleanUpAbandonedRunnerServices")
	logs.Info("start cleaning up abandoned runner services")
	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute*10)
	defer cancel()

	var bentoDeploymentNamespaces []string

	if cachedBentoDeploymentNamespaces != nil {
		bentoDeploymentNamespaces = *cachedBentoDeploymentNamespaces
	} else {
		restConfig := config.GetConfigOrDie()
		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return errors.Wrapf(err, "create kubernetes clientset")
		}

		bentoDeploymentNamespaces, err = commonconfig.GetBentoDeploymentNamespaces(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
			secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
			return secret, errors.Wrap(err, "get secret")
		})
		if err != nil {
			err = errors.Wrapf(err, "get bento deployment namespaces")
			return err
		}

		cachedBentoDeploymentNamespaces = &bentoDeploymentNamespaces
	}

	for _, bentoDeploymentNamespace := range bentoDeploymentNamespaces {
		serviceList := &corev1.ServiceList{}
		serviceListOpts := []client.ListOption{
			client.HasLabels{commonconsts.KubeLabelYataiBentoDeploymentRunner},
			client.InNamespace(bentoDeploymentNamespace),
		}
		err := r.List(ctx, serviceList, serviceListOpts...)
		if err != nil {
			return errors.Wrap(err, "list services")
		}
		for _, service := range serviceList.Items {
			service := service
			podList := &corev1.PodList{}
			podListOpts := []client.ListOption{
				client.InNamespace(service.Namespace),
				client.MatchingLabels(service.Spec.Selector),
			}
			err := r.List(ctx, podList, podListOpts...)
			if err != nil {
				return errors.Wrap(err, "list pods")
			}
			if len(podList.Items) > 0 {
				continue
			}
			createdAt := service.ObjectMeta.CreationTimestamp
			if time.Since(createdAt.Time) < time.Minute*3 {
				continue
			}
			logs.Info("deleting abandoned runner service", "name", service.Name, "namespace", service.Namespace)
			err = r.Delete(ctx, &service)
			if err != nil {
				return errors.Wrapf(err, "delete service %s", service.Name)
			}
		}
	}
	logs.Info("finished cleaning up abandoned runner services")
	return nil
}

func (r *BentoDeploymentReconciler) cleanUpAbandonedRunnerServices() {
	logs := log.Log.WithValues("func", "cleanUpAbandonedRunnerServices")
	err := r.doCleanUpAbandonedRunnerServices()
	if err != nil {
		logs.Error(err, "cleanUpAbandonedRunnerServices")
	}
	ticker := time.NewTicker(time.Second * 30)
	for range ticker.C {
		err := r.doCleanUpAbandonedRunnerServices()
		if err != nil {
			logs.Error(err, "cleanUpAbandonedRunnerServices")
		}
	}
}
