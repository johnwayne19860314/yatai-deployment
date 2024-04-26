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

	// nolint: gosec
	"crypto/md5"
	"encoding/hex"

	"github.com/huandu/xstrings"
	"github.com/pkg/errors"
	autoscalingv2beta2 "k8s.io/api/autoscaling/v2beta2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/bentoml/yatai-schemas/modelschemas"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"
	commonutils "github.com/bentoml/yatai-common/utils"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"

	servingcommon "github.com/bentoml/yatai-deployment/apis/serving/common"
	servingv2alpha1 "github.com/bentoml/yatai-deployment/apis/serving/v2alpha1"
	yataiclient "github.com/bentoml/yatai-deployment/yatai-client"
)

const (
	DefaultClusterName                                        = "default"
	DefaultServiceAccountName                                 = "default"
	KubeValueNameSharedMemory                                 = "shared-memory"
	KubeAnnotationDeploymentStrategy                          = "yatai.ai/deployment-strategy"
	KubeAnnotationYataiEnableStealingTrafficDebugMode         = "yatai.ai/enable-stealing-traffic-debug-mode"
	KubeAnnotationYataiEnableDebugMode                        = "yatai.ai/enable-debug-mode"
	KubeAnnotationYataiEnableDebugPodReceiveProductionTraffic = "yatai.ai/enable-debug-pod-receive-production-traffic"
	KubeAnnotationYataiProxySidecarResourcesLimitsCPU         = "yatai.ai/proxy-sidecar-resources-limits-cpu"
	KubeAnnotationYataiProxySidecarResourcesLimitsMemory      = "yatai.ai/proxy-sidecar-resources-limits-memory"
	KubeAnnotationYataiProxySidecarResourcesRequestsCPU       = "yatai.ai/proxy-sidecar-resources-requests-cpu"
	KubeAnnotationYataiProxySidecarResourcesRequestsMemory    = "yatai.ai/proxy-sidecar-resources-requests-memory"
	DeploymentTargetTypeProduction                            = "production"
	DeploymentTargetTypeDebug                                 = "debug"
	ContainerPortNameHTTPProxy                                = "http-proxy"
	ServicePortNameHTTPNonProxy                               = "http-non-proxy"
	HeaderNameDebug                                           = "X-Yatai-Debug"

	legacyHPAMajorVersion = "1"
	legacyHPAMinorVersion = "23"
)

var ServicePortHTTPNonProxy = commonconsts.BentoServicePort + 1

var cachedYataiConf *commonconfig.YataiConfig

type createOrUpdateOrDeleteDeploymentsOption struct {
	yataiClient     **yataiclient.YataiClient
	bentoDeployment *servingv2alpha1.BentoDeployment
	bento           *resourcesv1alpha1.Bento
	clusterName     *string
	runnerName      *string
}

type createOrUpdateDeploymentOption struct {
	createOrUpdateOrDeleteDeploymentsOption
	isStealingTrafficDebugModeEnabled       bool
	containsStealingTrafficDebugModeEnabled bool
}

func getResourceAnnotations(bentoDeployment *servingv2alpha1.BentoDeployment, runnerName *string) map[string]string {
	var resourceAnnotations map[string]string
	if runnerName != nil {
		for _, runner := range bentoDeployment.Spec.Runners {
			if runner.Name == *runnerName {
				resourceAnnotations = runner.Annotations
				break
			}
		}
	} else {
		resourceAnnotations = bentoDeployment.Spec.Annotations
	}

	if resourceAnnotations == nil {
		resourceAnnotations = map[string]string{}
	}

	return resourceAnnotations
}

func checkIfIsDebugModeEnabled(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}

	return annotations[KubeAnnotationYataiEnableDebugMode] == commonconsts.KubeLabelValueTrue
}

func checkIfIsStealingTrafficDebugModeEnabled(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}

	return annotations[KubeAnnotationYataiEnableStealingTrafficDebugMode] == commonconsts.KubeLabelValueTrue
}

func checkIfIsDebugPodReceiveProductionTrafficEnabled(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}

	return annotations[KubeAnnotationYataiEnableDebugPodReceiveProductionTraffic] == commonconsts.KubeLabelValueTrue
}

func checkIfContainsStealingTrafficDebugModeEnabled(bentoDeployment *servingv2alpha1.BentoDeployment) bool {
	if checkIfIsStealingTrafficDebugModeEnabled(bentoDeployment.Spec.Annotations) {
		return true
	}
	for _, runner := range bentoDeployment.Spec.Runners {
		if checkIfIsStealingTrafficDebugModeEnabled(runner.Annotations) {
			return true
		}
	}
	return false
}

type createOrUpdateOrDeleteServicesOption struct {
	bentoDeployment *servingv2alpha1.BentoDeployment
	bento           *resourcesv1alpha1.Bento
	runnerName      *string
}

type createOrUpdateServiceOption struct {
	bentoDeployment                         *servingv2alpha1.BentoDeployment
	bento                                   *resourcesv1alpha1.Bento
	runnerName                              *string
	isStealingTrafficDebugModeEnabled       bool
	isDebugPodReceiveProductionTraffic      bool
	containsStealingTrafficDebugModeEnabled bool
	isGenericService                        bool
}

type createOrUpdateIngressOption struct {
	yataiClient     **yataiclient.YataiClient
	bentoDeployment *servingv2alpha1.BentoDeployment
	bento           *resourcesv1alpha1.Bento
}

func hash(text string) string {
	// nolint: gosec
	hasher := md5.New()
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}

type generateDeploymentOption struct {
	bentoDeployment                         *servingv2alpha1.BentoDeployment
	bento                                   *resourcesv1alpha1.Bento
	yataiClient                             **yataiclient.YataiClient
	runnerName                              *string
	clusterName                             *string
	isStealingTrafficDebugModeEnabled       bool
	containsStealingTrafficDebugModeEnabled bool
}

func getBentoRepositoryNameAndBentoVersion(bento *resourcesv1alpha1.Bento) (repositoryName string, version string) {
	repositoryName, _, version = xstrings.Partition(bento.Spec.Tag, ":")

	return
}

type generatePodTemplateSpecOption struct {
	bentoDeployment                         *servingv2alpha1.BentoDeployment
	bento                                   *resourcesv1alpha1.Bento
	yataiClient                             **yataiclient.YataiClient
	clusterName                             *string
	runnerName                              *string
	isStealingTrafficDebugModeEnabled       bool
	containsStealingTrafficDebugModeEnabled bool
}

func getResourcesConfig(resources *servingcommon.Resources) (corev1.ResourceRequirements, error) {
	currentResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("300m"),
			corev1.ResourceMemory: resource.MustParse("500Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}

	if resources == nil {
		return currentResources, nil
	}

	if resources.Limits != nil {
		if resources.Limits.CPU != "" {
			q, err := resource.ParseQuantity(resources.Limits.CPU)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse limits cpu quantity")
			}
			if currentResources.Limits == nil {
				currentResources.Limits = make(corev1.ResourceList)
			}
			currentResources.Limits[corev1.ResourceCPU] = q
		}
		if resources.Limits.Memory != "" {
			q, err := resource.ParseQuantity(resources.Limits.Memory)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse limits memory quantity")
			}
			if currentResources.Limits == nil {
				currentResources.Limits = make(corev1.ResourceList)
			}
			currentResources.Limits[corev1.ResourceMemory] = q
		}
		if resources.Limits.GPU != "" {
			q, err := resource.ParseQuantity(resources.Limits.GPU)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse limits gpu quantity")
			}
			if currentResources.Limits == nil {
				currentResources.Limits = make(corev1.ResourceList)
			}
			currentResources.Limits[commonconsts.KubeResourceGPUNvidia] = q
		}
		for k, v := range resources.Limits.Custom {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse limits %s quantity", k)
			}
			if currentResources.Limits == nil {
				currentResources.Limits = make(corev1.ResourceList)
			}
			currentResources.Limits[corev1.ResourceName(k)] = q
		}
	}
	if resources.Requests != nil {
		if resources.Requests.CPU != "" {
			q, err := resource.ParseQuantity(resources.Requests.CPU)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse requests cpu quantity")
			}
			if currentResources.Requests == nil {
				currentResources.Requests = make(corev1.ResourceList)
			}
			currentResources.Requests[corev1.ResourceCPU] = q
		}
		if resources.Requests.Memory != "" {
			q, err := resource.ParseQuantity(resources.Requests.Memory)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse requests memory quantity")
			}
			if currentResources.Requests == nil {
				currentResources.Requests = make(corev1.ResourceList)
			}
			currentResources.Requests[corev1.ResourceMemory] = q
		}
		for k, v := range resources.Requests.Custom {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return currentResources, errors.Wrapf(err, "parse requests %s quantity", k)
			}
			if currentResources.Requests == nil {
				currentResources.Requests = make(corev1.ResourceList)
			}
			currentResources.Requests[corev1.ResourceName(k)] = q
		}
	}
	return currentResources, nil
}

type generateServiceOption struct {
	bentoDeployment                         *servingv2alpha1.BentoDeployment
	bento                                   *resourcesv1alpha1.Bento
	runnerName                              *string
	isStealingTrafficDebugModeEnabled       bool
	isDebugPodReceiveProductionTraffic      bool
	containsStealingTrafficDebugModeEnabled bool
	isGenericService                        bool
}

var cachedDomainSuffix *string

type IngressConfig struct {
	ClassName   *string
	Annotations map[string]string
	Path        string
	PathType    networkingv1.PathType
}

var cachedIngressConfig *IngressConfig

type generateIngressesOption struct {
	yataiClient     **yataiclient.YataiClient
	bentoDeployment *servingv2alpha1.BentoDeployment
	bento           *resourcesv1alpha1.Bento
}

var cachedBentoDeploymentNamespaces *[]string

func TransformToOldHPA(hpa *servingv2alpha1.Autoscaling) (oldHpa *modelschemas.DeploymentTargetHPAConf, err error) {
	if hpa == nil {
		return
	}

	oldHpa = &modelschemas.DeploymentTargetHPAConf{
		MinReplicas: commonutils.Int32Ptr(hpa.MinReplicas),
		MaxReplicas: commonutils.Int32Ptr(hpa.MaxReplicas),
	}

	for _, metric := range hpa.Metrics {
		if metric.Type == autoscalingv2beta2.PodsMetricSourceType {
			if metric.Pods == nil {
				continue
			}
			if metric.Pods.Metric.Name == commonconsts.KubeHPAQPSMetric {
				if metric.Pods.Target.Type != autoscalingv2beta2.UtilizationMetricType {
					continue
				}
				if metric.Pods.Target.AverageValue == nil {
					continue
				}
				qps := metric.Pods.Target.AverageValue.Value()
				oldHpa.QPS = &qps
			}
		} else if metric.Type == autoscalingv2beta2.ResourceMetricSourceType {
			if metric.Resource == nil {
				continue
			}
			if metric.Resource.Name == corev1.ResourceCPU {
				if metric.Resource.Target.Type != autoscalingv2beta2.UtilizationMetricType {
					continue
				}
				if metric.Resource.Target.AverageUtilization == nil {
					continue
				}
				cpu := *metric.Resource.Target.AverageUtilization
				oldHpa.CPU = &cpu
			} else if metric.Resource.Name == corev1.ResourceMemory {
				if metric.Resource.Target.Type != autoscalingv2beta2.UtilizationMetricType {
					continue
				}
				if metric.Resource.Target.AverageUtilization == nil {
					continue
				}
				memory := metric.Resource.Target.AverageValue.String()
				oldHpa.Memory = &memory
			}
		}
	}
	return
}
