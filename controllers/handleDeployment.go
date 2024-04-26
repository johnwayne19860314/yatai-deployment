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

	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	pep440version "github.com/aquasecurity/go-pep440-version"
	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"

	"github.com/bentoml/yatai-deployment/utils"
)

func (r *BentoDeploymentReconciler) createOrUpdateOrDeleteDeployments(ctx context.Context, opt createOrUpdateOrDeleteDeploymentsOption) (modified bool, err error) {
	resourceAnnotations := getResourceAnnotations(opt.bentoDeployment, opt.runnerName)
	isStealingTrafficDebugModeEnabled := checkIfIsStealingTrafficDebugModeEnabled(resourceAnnotations)
	containsStealingTrafficDebugModeEnabled := checkIfContainsStealingTrafficDebugModeEnabled(opt.bentoDeployment)
	modified, err = r.createOrUpdateDeployment(ctx, createOrUpdateDeploymentOption{
		createOrUpdateOrDeleteDeploymentsOption: opt,
		isStealingTrafficDebugModeEnabled:       false,
		containsStealingTrafficDebugModeEnabled: containsStealingTrafficDebugModeEnabled,
	})
	if err != nil {
		err = errors.Wrap(err, "create or update deployment")
		return
	}
	if (opt.runnerName == nil && containsStealingTrafficDebugModeEnabled) || (opt.runnerName != nil && isStealingTrafficDebugModeEnabled) {
		modified, err = r.createOrUpdateDeployment(ctx, createOrUpdateDeploymentOption{
			createOrUpdateOrDeleteDeploymentsOption: opt,
			isStealingTrafficDebugModeEnabled:       true,
			containsStealingTrafficDebugModeEnabled: containsStealingTrafficDebugModeEnabled,
		})
		if err != nil {
			err = errors.Wrap(err, "create or update deployment")
			return
		}
	} else {
		debugDeploymentName := r.getKubeName(opt.bentoDeployment, opt.bento, opt.runnerName, true)
		debugDeployment := &appsv1.Deployment{}
		err = r.Get(ctx, types.NamespacedName{Name: debugDeploymentName, Namespace: opt.bentoDeployment.Namespace}, debugDeployment)
		isNotFound := k8serrors.IsNotFound(err)
		if err != nil && !isNotFound {
			err = errors.Wrap(err, "get deployment")
			return
		}
		err = nil
		if !isNotFound {
			err = r.Delete(ctx, debugDeployment)
			if err != nil {
				err = errors.Wrap(err, "delete deployment")
				return
			}
			modified = true
		}
	}
	return
}

func (r *BentoDeploymentReconciler) createOrUpdateDeployment(ctx context.Context, opt createOrUpdateDeploymentOption) (modified bool, err error) {
	logs := log.FromContext(ctx)

	deployment, err := r.generateDeployment(ctx, generateDeploymentOption{
		bentoDeployment:                         opt.bentoDeployment,
		bento:                                   opt.bento,
		yataiClient:                             opt.yataiClient,
		clusterName:                             opt.clusterName,
		runnerName:                              opt.runnerName,
		isStealingTrafficDebugModeEnabled:       opt.isStealingTrafficDebugModeEnabled,
		containsStealingTrafficDebugModeEnabled: opt.containsStealingTrafficDebugModeEnabled,
	})
	if err != nil {
		return
	}

	logs = logs.WithValues("namespace", deployment.Namespace, "deploymentName", deployment.Name)

	deploymentNamespacedName := fmt.Sprintf("%s/%s", deployment.Namespace, deployment.Name)

	r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "GetDeployment", "Getting Deployment %s", deploymentNamespacedName)

	oldDeployment := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, oldDeployment)
	oldDeploymentIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !oldDeploymentIsNotFound {
		r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "GetDeployment", "Failed to get Deployment %s: %s", deploymentNamespacedName, err)
		logs.Error(err, "Failed to get Deployment.")
		return
	}

	if oldDeploymentIsNotFound {
		logs.Info("Deployment not found. Creating a new one.")

		err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(deployment), "set last applied annotation for deployment %s", deployment.Name)
		if err != nil {
			logs.Error(err, "Failed to set last applied annotation.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for Deployment %s: %s", deploymentNamespacedName, err)
			return
		}

		r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "CreateDeployment", "Creating a new Deployment %s", deploymentNamespacedName)
		err = r.Create(ctx, deployment)
		if err != nil {
			logs.Error(err, "Failed to create Deployment.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "CreateDeployment", "Failed to create Deployment %s: %s", deploymentNamespacedName, err)
			return
		}
		logs.Info("Deployment created.")
		r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "CreateDeployment", "Created Deployment %s", deploymentNamespacedName)
		modified = true
	} else {
		logs.Info("Deployment found.")

		var patchResult *patch.PatchResult
		patchResult, err = patch.DefaultPatchMaker.Calculate(oldDeployment, deployment)
		if err != nil {
			logs.Error(err, "Failed to calculate patch.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "CalculatePatch", "Failed to calculate patch for Deployment %s: %s", deploymentNamespacedName, err)
			return
		}

		if !patchResult.IsEmpty() {
			logs.Info("Deployment spec is different. Updating Deployment.")

			err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(deployment), "set last applied annotation for deployment %s", deployment.Name)
			if err != nil {
				logs.Error(err, "Failed to set last applied annotation.")
				r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for Deployment %s: %s", deploymentNamespacedName, err)
				return
			}

			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "UpdateDeployment", "Updating Deployment %s", deploymentNamespacedName)
			err = r.Update(ctx, deployment)
			if err != nil {
				logs.Error(err, "Failed to update Deployment.")
				r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeWarning, "UpdateDeployment", "Failed to update Deployment %s: %s", deploymentNamespacedName, err)
				return
			}
			logs.Info("Deployment updated.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "UpdateDeployment", "Updated Deployment %s", deploymentNamespacedName)
			modified = true
		} else {
			logs.Info("Deployment spec is the same. Skipping update.")
			r.Recorder.Eventf(opt.bentoDeployment, corev1.EventTypeNormal, "UpdateDeployment", "Skipping update Deployment %s", deploymentNamespacedName)
		}
	}

	return
}

func (r *BentoDeploymentReconciler) generateDeployment(ctx context.Context, opt generateDeploymentOption) (kubeDeployment *appsv1.Deployment, err error) {
	kubeNs := opt.bentoDeployment.Namespace

	// nolint: gosimple
	podTemplateSpec, err := r.generatePodTemplateSpec(ctx, generatePodTemplateSpecOption{
		bentoDeployment:                         opt.bentoDeployment,
		bento:                                   opt.bento,
		yataiClient:                             opt.yataiClient,
		runnerName:                              opt.runnerName,
		clusterName:                             opt.clusterName,
		isStealingTrafficDebugModeEnabled:       opt.isStealingTrafficDebugModeEnabled,
		containsStealingTrafficDebugModeEnabled: opt.containsStealingTrafficDebugModeEnabled,
	})
	if err != nil {
		return
	}

	labels := r.getKubeLabels(opt.bentoDeployment, opt.bento, opt.runnerName)

	annotations := r.getKubeAnnotations(opt.bentoDeployment, opt.bento, opt.runnerName)

	kubeName := r.getKubeName(opt.bentoDeployment, opt.bento, opt.runnerName, opt.isStealingTrafficDebugModeEnabled)

	defaultMaxSurge := intstr.FromString("25%")
	defaultMaxUnavailable := intstr.FromString("25%")

	strategy := appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &defaultMaxSurge,
			MaxUnavailable: &defaultMaxUnavailable,
		},
	}

	resourceAnnotations := getResourceAnnotations(opt.bentoDeployment, opt.runnerName)
	strategyStr := resourceAnnotations[KubeAnnotationDeploymentStrategy]
	if strategyStr != "" {
		strategyType := modelschemas.DeploymentStrategy(strategyStr)
		switch strategyType {
		case modelschemas.DeploymentStrategyRollingUpdate:
			strategy = appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &defaultMaxSurge,
					MaxUnavailable: &defaultMaxUnavailable,
				},
			}
		case modelschemas.DeploymentStrategyRecreate:
			strategy = appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			}
		case modelschemas.DeploymentStrategyRampedSlowRollout:
			strategy = appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &[]intstr.IntOrString{intstr.FromInt(1)}[0],
					MaxUnavailable: &[]intstr.IntOrString{intstr.FromInt(0)}[0],
				},
			}
		case modelschemas.DeploymentStrategyBestEffortControlledRollout:
			strategy = appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &[]intstr.IntOrString{intstr.FromInt(0)}[0],
					MaxUnavailable: &[]intstr.IntOrString{intstr.FromString("20%")}[0],
				},
			}
		}
	}

	var replicas *int32
	if opt.isStealingTrafficDebugModeEnabled {
		replicas = &[]int32{int32(1)}[0]
	}

	kubeDeployment = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        kubeName,
			Namespace:   kubeNs,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					commonconsts.KubeLabelYataiSelector: kubeName,
				},
			},
			Template: *podTemplateSpec,
			Strategy: strategy,
		},
	}

	err = ctrl.SetControllerReference(opt.bentoDeployment, kubeDeployment, r.Scheme)
	if err != nil {
		err = errors.Wrapf(err, "set deployment %s controller reference", kubeDeployment.Name)
	}

	return
}

func (r *BentoDeploymentReconciler) generatePodTemplateSpec(ctx context.Context, opt generatePodTemplateSpecOption) (podTemplateSpec *corev1.PodTemplateSpec, err error) {
	bentoRepositoryName, bentoVersion := getBentoRepositoryNameAndBentoVersion(opt.bento)
	podLabels := r.getKubeLabels(opt.bentoDeployment, opt.bento, opt.runnerName)
	if opt.runnerName != nil {
		podLabels[commonconsts.KubeLabelBentoRepository] = bentoRepositoryName
		podLabels[commonconsts.KubeLabelBentoVersion] = bentoVersion
	}
	if opt.isStealingTrafficDebugModeEnabled {
		podLabels[commonconsts.KubeLabelYataiBentoDeploymentTargetType] = DeploymentTargetTypeDebug
	}

	podAnnotations := r.getKubeAnnotations(opt.bentoDeployment, opt.bento, opt.runnerName)

	kubeName := r.getKubeName(opt.bentoDeployment, opt.bento, opt.runnerName, opt.isStealingTrafficDebugModeEnabled)

	containerPort := commonconsts.BentoServicePort
	lastPort := containerPort + 1

	monitorExporter := opt.bentoDeployment.Spec.MonitorExporter
	needMonitorContainer := monitorExporter != nil && monitorExporter.Enabled && opt.runnerName == nil

	lastPort++
	monitorExporterPort := lastPort

	var envs []corev1.EnvVar
	envsSeen := make(map[string]struct{})

	var resourceAnnotations map[string]string
	var specEnvs []corev1.EnvVar
	if opt.runnerName != nil {
		for _, runner := range opt.bentoDeployment.Spec.Runners {
			if runner.Name == *opt.runnerName {
				specEnvs = runner.Envs
				resourceAnnotations = runner.Annotations
				break
			}
		}
	} else {
		specEnvs = opt.bentoDeployment.Spec.Envs
		resourceAnnotations = opt.bentoDeployment.Spec.Annotations
	}

	if resourceAnnotations == nil {
		resourceAnnotations = make(map[string]string)
	}

	isDebugModeEnabled := checkIfIsDebugModeEnabled(resourceAnnotations)

	if specEnvs != nil {
		envs = make([]corev1.EnvVar, 0, len(specEnvs)+1)

		for _, env := range specEnvs {
			if _, ok := envsSeen[env.Name]; ok {
				continue
			}
			if env.Name == commonconsts.EnvBentoServicePort {
				// nolint: gosec
				containerPort, err = strconv.Atoi(env.Value)
				if err != nil {
					return nil, errors.Wrapf(err, "invalid port value %s", env.Value)
				}
			}
			envsSeen[env.Name] = struct{}{}
			envs = append(envs, corev1.EnvVar{
				Name:  env.Name,
				Value: env.Value,
			})
		}
	}

	defaultEnvs := []corev1.EnvVar{
		{
			Name:  commonconsts.EnvBentoServicePort,
			Value: fmt.Sprintf("%d", containerPort),
		},
		{
			Name:  commonconsts.EnvYataiDeploymentUID,
			Value: string(opt.bentoDeployment.UID),
		},
		{
			Name:  commonconsts.EnvYataiBentoDeploymentName,
			Value: opt.bentoDeployment.Name,
		},
		{
			Name:  commonconsts.EnvYataiBentoDeploymentNamespace,
			Value: opt.bentoDeployment.Namespace,
		},
	}

	if opt.yataiClient != nil {
		yataiClient := *opt.yataiClient
		var organization *schemasv1.OrganizationFullSchema
		organization, err = yataiClient.GetOrganization(ctx)
		if err != nil {
			return
		}

		var cluster *schemasv1.ClusterFullSchema
		clusterName := DefaultClusterName
		if opt.clusterName != nil {
			clusterName = *opt.clusterName
		}
		cluster, err = yataiClient.GetCluster(ctx, clusterName)
		if err != nil {
			return
		}

		var version *schemasv1.VersionSchema
		version, err = yataiClient.GetVersion(ctx)
		if err != nil {
			return
		}

		defaultEnvs = append(defaultEnvs, []corev1.EnvVar{
			{
				Name:  commonconsts.EnvYataiVersion,
				Value: fmt.Sprintf("%s-%s", version.Version, version.GitCommit),
			},
			{
				Name:  commonconsts.EnvYataiOrgUID,
				Value: organization.Uid,
			},
			{
				Name:  commonconsts.EnvYataiClusterUID,
				Value: cluster.Uid,
			},
		}...)
	}

	for _, env := range defaultEnvs {
		if _, ok := envsSeen[env.Name]; !ok {
			envs = append(envs, env)
		}
	}

	if needMonitorContainer {
		monitoringConfigTemplate := `monitoring.enabled=true
monitoring.type=otlp
monitoring.options.endpoint=http://127.0.0.1:%d
monitoring.options.insecure=true`
		var bentomlOptions string
		index := -1
		for i, env := range envs {
			if env.Name == "BENTOML_CONFIG_OPTIONS" {
				bentomlOptions = env.Value
				index = i
				break
			}
		}
		if index == -1 {
			// BENOML_CONFIG_OPTIONS not defined
			bentomlOptions = fmt.Sprintf(monitoringConfigTemplate, monitorExporterPort)
			envs = append(envs, corev1.EnvVar{
				Name:  "BENTOML_CONFIG_OPTIONS",
				Value: bentomlOptions,
			})
		} else if !strings.Contains(bentomlOptions, "monitoring") {
			// monitoring config not defined
			envs = append(envs[:index], envs[index+1:]...)
			bentomlOptions = strings.TrimSpace(bentomlOptions) // ' ' -> ''
			if bentomlOptions != "" {
				bentomlOptions += "\n"
			}
			bentomlOptions += fmt.Sprintf(monitoringConfigTemplate, monitorExporterPort)
			envs = append(envs, corev1.EnvVar{
				Name:  "BENTOML_CONFIG_OPTIONS",
				Value: bentomlOptions,
			})
		}
		// monitoring config already defined
		// do nothing
	}

	livenessProbe := &corev1.Probe{
		InitialDelaySeconds: 10,
		TimeoutSeconds:      20,
		FailureThreshold:    6,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/livez",
				Port: intstr.FromString(commonconsts.BentoContainerPortName),
			},
		},
	}

	readinessProbe := &corev1.Probe{
		InitialDelaySeconds: 5,
		TimeoutSeconds:      5,
		FailureThreshold:    12,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz",
				Port: intstr.FromString(commonconsts.BentoContainerPortName),
			},
		},
	}

	volumes := make([]corev1.Volume, 0)
	volumeMounts := make([]corev1.VolumeMount, 0)

	args := make([]string, 0)

	isOldVersion := false
	if opt.bento.Spec.Context != nil && opt.bento.Spec.Context.BentomlVersion != "" {
		var currentVersion pep440version.Version
		currentVersion, err = pep440version.Parse(opt.bento.Spec.Context.BentomlVersion)
		if err != nil {
			err = errors.Wrapf(err, "invalid bentoml version %s", opt.bento.Spec.Context.BentomlVersion)
			return
		}
		var targetVersion pep440version.Version
		targetVersion, err = pep440version.Parse("1.0.0a7")
		if err != nil {
			err = errors.Wrapf(err, "invalid target version %s", opt.bento.Spec.Context.BentomlVersion)
			return
		}
		isOldVersion = currentVersion.LessThanOrEqual(targetVersion)
	}

	if opt.runnerName != nil {
		// python -m bentoml._internal.server.cli.runner iris_classifier:ohzovcfvvseu3lg6 iris_clf tcp://127.0.0.1:8001 --working-dir .
		if isOldVersion {
			args = append(args, "./env/docker/entrypoint.sh", "python", "-m", "bentoml._internal.server.cli.runner", ".", *opt.runnerName, fmt.Sprintf("tcp://0.0.0.0:%d", containerPort), "--working-dir", ".")
		} else {
			args = append(args, "./env/docker/entrypoint.sh", "python", "-m", "bentoml._internal.server.cli.runner", ".", "--runner-name", *opt.runnerName, "--bind", fmt.Sprintf("tcp://0.0.0.0:%d", containerPort), "--working-dir", ".")
		}
	} else {
		if len(opt.bento.Spec.Runners) > 0 {
			readinessProbeUrls := make([]string, 0)
			livenessProbeUrls := make([]string, 0)
			readinessProbeUrls = append(readinessProbeUrls, fmt.Sprintf("http://localhost:%d/readyz", containerPort))
			livenessProbeUrls = append(livenessProbeUrls, fmt.Sprintf("http://localhost:%d/healthz", containerPort))
			// python -m bentoml._internal.server.cli.api_server  iris_classifier:ohzovcfvvseu3lg6 tcp://127.0.0.1:8000 --runner-map '{"iris_clf": "tcp://127.0.0.1:8001"}' --working-dir .
			runnerMap := make(map[string]string, len(opt.bento.Spec.Runners))
			for _, runner := range opt.bento.Spec.Runners {
				isRunnerStealingTrafficDebugModeEnabled := false
				if opt.bentoDeployment.Spec.Runners != nil {
					for _, runnerResource := range opt.bentoDeployment.Spec.Runners {
						if runnerResource.Name == runner.Name {
							isRunnerStealingTrafficDebugModeEnabled = checkIfIsStealingTrafficDebugModeEnabled(runnerResource.Annotations)
							break
						}
					}
				}
				var runnerServiceName string
				if opt.isStealingTrafficDebugModeEnabled {
					runnerServiceName = r.getRunnerServiceName(opt.bentoDeployment, opt.bento, runner.Name, isRunnerStealingTrafficDebugModeEnabled)
				} else {
					runnerServiceName = r.getRunnerGenericServiceName(opt.bentoDeployment, opt.bento, runner.Name)
				}
				runnerMap[runner.Name] = fmt.Sprintf("tcp://%s:%d", runnerServiceName, commonconsts.BentoServicePort)
				readinessProbeUrls = append(readinessProbeUrls, fmt.Sprintf("http://%s:%d/readyz", runnerServiceName, commonconsts.BentoServicePort))
				livenessProbeUrls = append(livenessProbeUrls, fmt.Sprintf("http://%s:%d/healthz", runnerServiceName, commonconsts.BentoServicePort))
			}

			livenessProbePythonCommandPieces := make([]string, 0, len(opt.bento.Spec.Runners)+1)
			for _, url_ := range livenessProbeUrls {
				livenessProbePythonCommandPieces = append(livenessProbePythonCommandPieces, fmt.Sprintf("urlopen('%s')", url_))
			}

			readinessProbePythonCommandPieces := make([]string, 0, len(opt.bento.Spec.Runners)+1)
			for _, url_ := range readinessProbeUrls {
				readinessProbePythonCommandPieces = append(readinessProbePythonCommandPieces, fmt.Sprintf("urlopen('%s')", url_))
			}

			livenessProbe = &corev1.Probe{
				InitialDelaySeconds: 5,
				TimeoutSeconds:      5,
				FailureThreshold:    6,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"python3",
							"-c",
							fmt.Sprintf(`from urllib.request import urlopen; %s`, strings.Join(livenessProbePythonCommandPieces, "; ")),
						},
					},
				},
			}

			readinessProbe = &corev1.Probe{
				InitialDelaySeconds: 5,
				TimeoutSeconds:      5,
				FailureThreshold:    36,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"python3",
							"-c",
							fmt.Sprintf(`from urllib.request import urlopen; %s`, strings.Join(readinessProbePythonCommandPieces, "; ")),
						},
					},
				},
			}

			runnerMapStr, err := json.Marshal(runnerMap)
			if err != nil {
				return nil, errors.Wrap(err, "failed to marshal runner map")
			}
			if isOldVersion {
				args = append(args, "./env/docker/entrypoint.sh", "python", "-m", "bentoml._internal.server.cli.api_server", ".", fmt.Sprintf("tcp://0.0.0.0:%d", containerPort), "--runner-map", fmt.Sprintf("'%s'", string(runnerMapStr)), "--working-dir", ".")
			} else {
				args = append(args, "./env/docker/entrypoint.sh", "python", "-m", "bentoml._internal.server.cli.api_server", ".", "--bind", fmt.Sprintf("tcp://0.0.0.0:%d", containerPort), "--runner-map", fmt.Sprintf("'%s'", string(runnerMapStr)), "--working-dir", ".")
			}
		} else {
			args = append(args, "./env/docker/entrypoint.sh", "bentoml", "serve", ".", "--production")
		}
	}

	yataiResources := opt.bentoDeployment.Spec.Resources
	if opt.runnerName != nil {
		for _, runner := range opt.bentoDeployment.Spec.Runners {
			if runner.Name != *opt.runnerName {
				continue
			}
			yataiResources = runner.Resources
			break
		}
	}

	resources, err := getResourcesConfig(yataiResources)
	if err != nil {
		err = errors.Wrap(err, "failed to get resources config")
		return nil, err
	}

	sharedMemorySizeLimit := resource.MustParse("64Mi")
	memoryLimit := resources.Limits[corev1.ResourceMemory]
	if !memoryLimit.IsZero() {
		sharedMemorySizeLimit.SetMilli(memoryLimit.MilliValue() / 2)
	}

	volumes = append(volumes, corev1.Volume{
		Name: KubeValueNameSharedMemory,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium:    corev1.StorageMediumMemory,
				SizeLimit: &sharedMemorySizeLimit,
			},
		},
	})
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      KubeValueNameSharedMemory,
		MountPath: "/dev/shm",
	})

	imageName := opt.bento.Spec.Image

	var securityContext *corev1.SecurityContext
	var mainContainerSecurityContext *corev1.SecurityContext

	enableRestrictedSecurityContext := os.Getenv("ENABLE_RESTRICTED_SECURITY_CONTEXT") == "true"
	if enableRestrictedSecurityContext {
		securityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: pointer.BoolPtr(false),
			RunAsNonRoot:             pointer.BoolPtr(true),
			RunAsUser:                pointer.Int64Ptr(1000),
			RunAsGroup:               pointer.Int64Ptr(1000),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		}
		mainContainerSecurityContext = securityContext.DeepCopy()
		mainContainerSecurityContext.RunAsUser = pointer.Int64Ptr(1034)
	}

	containers := make([]corev1.Container, 0, 2)

	container := corev1.Container{
		Name:           "main",
		Image:          imageName,
		Command:        []string{"sh", "-c"},
		Args:           []string{strings.Join(args, " ")},
		LivenessProbe:  livenessProbe,
		ReadinessProbe: readinessProbe,
		Resources:      resources,
		Env:            envs,
		TTY:            true,
		Stdin:          true,
		VolumeMounts:   volumeMounts,
		Ports: []corev1.ContainerPort{
			{
				Protocol:      corev1.ProtocolTCP,
				Name:          commonconsts.BentoContainerPortName,
				ContainerPort: int32(containerPort), // nolint: gosec
			},
		},
		SecurityContext: mainContainerSecurityContext,
	}

	if resourceAnnotations["yatai.ai/enable-container-privileged"] == commonconsts.KubeLabelValueTrue {
		if container.SecurityContext == nil {
			container.SecurityContext = &corev1.SecurityContext{}
		}
		container.SecurityContext.Privileged = &[]bool{true}[0]
	}

	if resourceAnnotations["yatai.ai/enable-container-ptrace"] == commonconsts.KubeLabelValueTrue {
		if container.SecurityContext == nil {
			container.SecurityContext = &corev1.SecurityContext{}
		}
		container.SecurityContext.Capabilities = &corev1.Capabilities{
			Add: []corev1.Capability{"SYS_PTRACE"},
		}
	}

	if resourceAnnotations["yatai.ai/run-container-as-root"] == commonconsts.KubeLabelValueTrue {
		if container.SecurityContext == nil {
			container.SecurityContext = &corev1.SecurityContext{}
		}
		container.SecurityContext.RunAsUser = &[]int64{0}[0]
	}

	containers = append(containers, container)

	lastPort++
	metricsPort := lastPort

	containers = append(containers, corev1.Container{
		Name:  "metrics-transformer",
		Image: commonconfig.GetInternalImages().MetricsTransformer,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("10Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		},
		ReadinessProbe: &corev1.Probe{
			InitialDelaySeconds: 5,
			TimeoutSeconds:      5,
			FailureThreshold:    10,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromString("metrics"),
				},
			},
		},
		LivenessProbe: &corev1.Probe{
			InitialDelaySeconds: 5,
			TimeoutSeconds:      5,
			FailureThreshold:    10,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromString("metrics"),
				},
			},
		},
		Env: []corev1.EnvVar{
			{
				Name:  "BENTOML_SERVER_HOST",
				Value: "localhost",
			},
			{
				Name:  "BENTOML_SERVER_PORT",
				Value: fmt.Sprintf("%d", containerPort),
			},
			{
				Name:  "PORT",
				Value: fmt.Sprintf("%d", metricsPort),
			},
			{
				Name:  "OLD_METRICS_PREFIX",
				Value: fmt.Sprintf("BENTOML_%s_", strings.ReplaceAll(bentoRepositoryName, "-", ":")),
			},
			{
				Name:  "NEW_METRICS_PREFIX",
				Value: "BENTOML_",
			},
		},
		Ports: []corev1.ContainerPort{
			{
				Protocol:      corev1.ProtocolTCP,
				Name:          "metrics",
				ContainerPort: int32(metricsPort),
			},
		},
		SecurityContext: securityContext,
	})

	if opt.runnerName == nil {
		lastPort++
		proxyPort := lastPort

		proxyResourcesRequestsCPUStr := resourceAnnotations[KubeAnnotationYataiProxySidecarResourcesRequestsCPU]
		if proxyResourcesRequestsCPUStr == "" {
			proxyResourcesRequestsCPUStr = "100m"
		}
		var proxyResourcesRequestsCPU resource.Quantity
		proxyResourcesRequestsCPU, err = resource.ParseQuantity(proxyResourcesRequestsCPUStr)
		if err != nil {
			err = errors.Wrapf(err, "failed to parse proxy sidecar resources requests cpu: %s", proxyResourcesRequestsCPUStr)
			return nil, err
		}
		proxyResourcesRequestsMemoryStr := resourceAnnotations[KubeAnnotationYataiProxySidecarResourcesRequestsMemory]
		if proxyResourcesRequestsMemoryStr == "" {
			proxyResourcesRequestsMemoryStr = "200Mi"
		}
		var proxyResourcesRequestsMemory resource.Quantity
		proxyResourcesRequestsMemory, err = resource.ParseQuantity(proxyResourcesRequestsMemoryStr)
		if err != nil {
			err = errors.Wrapf(err, "failed to parse proxy sidecar resources requests memory: %s", proxyResourcesRequestsMemoryStr)
			return nil, err
		}
		proxyResourcesLimitsCPUStr := resourceAnnotations[KubeAnnotationYataiProxySidecarResourcesLimitsCPU]
		if proxyResourcesLimitsCPUStr == "" {
			proxyResourcesLimitsCPUStr = "300m"
		}
		var proxyResourcesLimitsCPU resource.Quantity
		proxyResourcesLimitsCPU, err = resource.ParseQuantity(proxyResourcesLimitsCPUStr)
		if err != nil {
			err = errors.Wrapf(err, "failed to parse proxy sidecar resources limits cpu: %s", proxyResourcesLimitsCPUStr)
			return nil, err
		}
		proxyResourcesLimitsMemoryStr := resourceAnnotations[KubeAnnotationYataiProxySidecarResourcesLimitsMemory]
		if proxyResourcesLimitsMemoryStr == "" {
			proxyResourcesLimitsMemoryStr = "1000Mi"
		}
		var proxyResourcesLimitsMemory resource.Quantity
		proxyResourcesLimitsMemory, err = resource.ParseQuantity(proxyResourcesLimitsMemoryStr)
		if err != nil {
			err = errors.Wrapf(err, "failed to parse proxy sidecar resources limits memory: %s", proxyResourcesLimitsMemoryStr)
			return nil, err
		}
		var envoyConfigContent string
		if opt.isStealingTrafficDebugModeEnabled {
			productionServiceName := r.getServiceName(opt.bentoDeployment, opt.bento, nil, false)
			envoyConfigContent, err = utils.GenerateEnvoyConfigurationContent(utils.CreateEnvoyConfig{
				ListenPort:              proxyPort,
				DebugHeaderName:         HeaderNameDebug,
				DebugHeaderValue:        commonconsts.KubeLabelValueTrue,
				DebugServerAddress:      "localhost",
				DebugServerPort:         containerPort,
				ProductionServerAddress: fmt.Sprintf("%s.%s.svc.cluster.local", productionServiceName, opt.bentoDeployment.Namespace),
				ProductionServerPort:    ServicePortHTTPNonProxy,
			})
		} else {
			debugServiceName := r.getServiceName(opt.bentoDeployment, opt.bento, nil, true)
			envoyConfigContent, err = utils.GenerateEnvoyConfigurationContent(utils.CreateEnvoyConfig{
				ListenPort:              proxyPort,
				DebugHeaderName:         HeaderNameDebug,
				DebugHeaderValue:        commonconsts.KubeLabelValueTrue,
				DebugServerAddress:      fmt.Sprintf("%s.%s.svc.cluster.local", debugServiceName, opt.bentoDeployment.Namespace),
				DebugServerPort:         ServicePortHTTPNonProxy,
				ProductionServerAddress: "localhost",
				ProductionServerPort:    containerPort,
			})
		}
		if err != nil {
			err = errors.Wrapf(err, "failed to generate envoy configuration content")
			return nil, err
		}
		envoyConfigConfigMapName := fmt.Sprintf("%s-envoy-config", kubeName)
		envoyConfigConfigMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      envoyConfigConfigMapName,
				Namespace: opt.bentoDeployment.Namespace,
			},
			Data: map[string]string{
				"envoy.yaml": envoyConfigContent,
			},
		}
		err = ctrl.SetControllerReference(opt.bentoDeployment, envoyConfigConfigMap, r.Scheme)
		if err != nil {
			err = errors.Wrapf(err, "failed to set controller reference for envoy config config map")
			return nil, err
		}
		_, err = ctrl.CreateOrUpdate(ctx, r.Client, envoyConfigConfigMap, func() error {
			envoyConfigConfigMap.Data["envoy.yaml"] = envoyConfigContent
			return nil
		})
		if err != nil {
			err = errors.Wrapf(err, "failed to create or update envoy config configmap")
			return nil, err
		}
		volumes = append(volumes, corev1.Volume{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: envoyConfigConfigMapName,
					},
				},
			},
		})
		proxyImage := "quay.io/bentoml/bentoml-proxy:0.0.1"
		proxyImage_ := os.Getenv("INTERNAL_IMAGES_PROXY")
		if proxyImage_ != "" {
			proxyImage = proxyImage_
		}
		containers = append(containers, corev1.Container{
			Name:  "proxy",
			Image: proxyImage,
			Command: []string{
				"envoy",
				"--config-path",
				"/etc/envoy/envoy.yaml",
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "envoy-config",
					MountPath: "/etc/envoy",
				},
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          ContainerPortNameHTTPProxy,
					ContainerPort: int32(proxyPort),
					Protocol:      corev1.ProtocolTCP,
				},
			},
			ReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				TimeoutSeconds:      5,
				FailureThreshold:    10,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"curl -s localhost:9901/server_info | grep state | grep -q LIVE",
						},
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				TimeoutSeconds:      5,
				FailureThreshold:    10,
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"curl -s localhost:9901/server_info | grep state | grep -q LIVE",
						},
					},
				},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    proxyResourcesRequestsCPU,
					corev1.ResourceMemory: proxyResourcesRequestsMemory,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    proxyResourcesLimitsCPU,
					corev1.ResourceMemory: proxyResourcesLimitsMemory,
				},
			},
			SecurityContext: securityContext,
		})
	}

	if needMonitorContainer {
		lastPort++
		monitorExporterProbePort := lastPort

		monitorExporterImage := "quay.io/bentoml/bentoml-monitor-exporter:0.0.3"
		monitorExporterImage_ := os.Getenv("INTERNAL_IMAGES_MONITOR_EXPORTER")
		if monitorExporterImage_ != "" {
			monitorExporterImage = monitorExporterImage_
		}

		monitorOptEnvs := make([]corev1.EnvVar, 0, len(monitorExporter.Options)+len(monitorExporter.StructureOptions))
		monitorOptEnvsSeen := make(map[string]struct{})

		for _, env := range monitorExporter.StructureOptions {
			monitorOptEnvsSeen[strings.ToLower(env.Name)] = struct{}{}
			monitorOptEnvs = append(monitorOptEnvs, corev1.EnvVar{
				Name:      "FLUENTBIT_OUTPUT_OPTION_" + strings.ToUpper(env.Name),
				Value:     env.Value,
				ValueFrom: env.ValueFrom,
			})
		}

		for k, v := range monitorExporter.Options {
			if _, exists := monitorOptEnvsSeen[strings.ToLower(k)]; exists {
				continue
			}
			monitorOptEnvs = append(monitorOptEnvs, corev1.EnvVar{
				Name:  "FLUENTBIT_OUTPUT_OPTION_" + strings.ToUpper(k),
				Value: v,
			})
		}

		monitorVolumeMounts := make([]corev1.VolumeMount, 0, len(monitorExporter.Mounts))
		for idx, mount := range monitorExporter.Mounts {
			volumeName := fmt.Sprintf("monitor-exporter-%d", idx)
			volumes = append(volumes, corev1.Volume{
				Name:         volumeName,
				VolumeSource: mount.VolumeSource,
			})
			monitorVolumeMounts = append(monitorVolumeMounts, corev1.VolumeMount{
				Name:      volumeName,
				MountPath: mount.Path,
				ReadOnly:  mount.ReadOnly,
			})
		}

		containers = append(containers, corev1.Container{
			Name:         "monitor-exporter",
			Image:        monitorExporterImage,
			VolumeMounts: monitorVolumeMounts,
			Env: append([]corev1.EnvVar{
				{
					Name:  "FLUENTBIT_OTLP_PORT",
					Value: fmt.Sprint(monitorExporterPort),
				},
				{
					Name:  "FLUENTBIT_HTTP_PORT",
					Value: fmt.Sprint(monitorExporterProbePort),
				},
				{
					Name:  "FLUENTBIT_OUTPUT",
					Value: monitorExporter.Output,
				},
			}, monitorOptEnvs...),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("24Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1000m"),
					corev1.ResourceMemory: resource.MustParse("72Mi"),
				},
			},
			ReadinessProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				TimeoutSeconds:      5,
				FailureThreshold:    10,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/readyz",
						Port: intstr.FromInt(monitorExporterProbePort),
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				TimeoutSeconds:      5,
				FailureThreshold:    10,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/livez",
						Port: intstr.FromInt(monitorExporterProbePort),
					},
				},
			},
			SecurityContext: securityContext,
		})
	}

	debuggerImage := "quay.io/bentoml/bento-debugger:0.0.8"
	debuggerImage_ := os.Getenv("INTERNAL_IMAGES_DEBUGGER")
	if debuggerImage_ != "" {
		debuggerImage = debuggerImage_
	}

	if opt.isStealingTrafficDebugModeEnabled || isDebugModeEnabled {
		containers = append(containers, corev1.Container{
			Name:  "debugger",
			Image: debuggerImage,
			Command: []string{
				"sleep",
				"infinity",
			},
			SecurityContext: &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{
					Add: []corev1.Capability{"SYS_PTRACE"},
				},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1000m"),
					corev1.ResourceMemory: resource.MustParse("1000Mi"),
				},
			},
			Stdin: true,
			TTY:   true,
		})
	}

	podLabels[commonconsts.KubeLabelYataiSelector] = kubeName

	podSpec := corev1.PodSpec{
		Containers: containers,
		Volumes:    volumes,
	}

	podSpec.ImagePullSecrets = opt.bento.Spec.ImagePullSecrets

	extraPodMetadata := opt.bentoDeployment.Spec.ExtraPodMetadata

	if opt.runnerName != nil {
		for _, runner := range opt.bentoDeployment.Spec.Runners {
			if runner.Name != *opt.runnerName {
				continue
			}
			extraPodMetadata = runner.ExtraPodMetadata
			break
		}
	}

	if extraPodMetadata != nil {
		for k, v := range extraPodMetadata.Annotations {
			podAnnotations[k] = v
		}

		for k, v := range extraPodMetadata.Labels {
			podLabels[k] = v
		}
	}

	extraPodSpec := opt.bentoDeployment.Spec.ExtraPodSpec

	if opt.runnerName != nil {
		for _, runner := range opt.bentoDeployment.Spec.Runners {
			if runner.Name != *opt.runnerName {
				continue
			}
			extraPodSpec = runner.ExtraPodSpec
			break
		}
	}

	if extraPodSpec != nil {
		podSpec.SchedulerName = extraPodSpec.SchedulerName
		podSpec.NodeSelector = extraPodSpec.NodeSelector
		podSpec.Affinity = extraPodSpec.Affinity
		podSpec.Tolerations = extraPodSpec.Tolerations
		podSpec.TopologySpreadConstraints = extraPodSpec.TopologySpreadConstraints
		podSpec.Containers = append(podSpec.Containers, extraPodSpec.Containers...)
		podSpec.ServiceAccountName = extraPodSpec.ServiceAccountName
	}

	if podSpec.ServiceAccountName == "" {
		serviceAccounts := &corev1.ServiceAccountList{}
		err = r.List(ctx, serviceAccounts, client.InNamespace(opt.bentoDeployment.Namespace), client.MatchingLabels{
			commonconsts.KubeLabelBentoDeploymentPod: commonconsts.KubeLabelValueTrue,
		})
		if err != nil {
			err = errors.Wrapf(err, "failed to list service accounts in namespace %s", opt.bentoDeployment.Namespace)
			return
		}
		if len(serviceAccounts.Items) > 0 {
			podSpec.ServiceAccountName = serviceAccounts.Items[0].Name
		} else {
			podSpec.ServiceAccountName = DefaultServiceAccountName
		}
	}

	if resourceAnnotations["yatai.ai/enable-host-ipc"] == commonconsts.KubeLabelValueTrue {
		podSpec.HostIPC = true
	}

	if resourceAnnotations["yatai.ai/enable-host-network"] == commonconsts.KubeLabelValueTrue {
		podSpec.HostNetwork = true
	}

	if resourceAnnotations["yatai.ai/enable-host-pid"] == commonconsts.KubeLabelValueTrue {
		podSpec.HostPID = true
	}

	if opt.isStealingTrafficDebugModeEnabled || isDebugModeEnabled {
		podSpec.ShareProcessNamespace = &[]bool{true}[0]
	}

	podTemplateSpec = &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      podLabels,
			Annotations: podAnnotations,
		},
		Spec: podSpec,
	}

	return
}
