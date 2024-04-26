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

	"github.com/huandu/xstrings"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"

	servingv2alpha1 "github.com/bentoml/yatai-deployment/apis/serving/v2alpha1"
	"github.com/bentoml/yatai-deployment/version"
	yataiclient "github.com/bentoml/yatai-deployment/yatai-client"
)

func (r *BentoDeploymentReconciler) setStatusConditions(ctx context.Context, req ctrl.Request, conditions ...metav1.Condition) (bentoDeployment *servingv2alpha1.BentoDeployment, err error) {
	bentoDeployment = &servingv2alpha1.BentoDeployment{}
	for i := 0; i < 3; i++ {
		if err = r.Get(ctx, req.NamespacedName, bentoDeployment); err != nil {
			err = errors.Wrap(err, "Failed to re-fetch BentoDeployment")
			return
		}
		for _, condition := range conditions {
			meta.SetStatusCondition(&bentoDeployment.Status.Conditions, condition)
		}
		if err = r.Status().Update(ctx, bentoDeployment); err != nil {
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}
	if err != nil {
		err = errors.Wrap(err, "Failed to update BentoDeployment status")
		return
	}
	if err = r.Get(ctx, req.NamespacedName, bentoDeployment); err != nil {
		err = errors.Wrap(err, "Failed to re-fetch BentoDeployment")
		return
	}
	return
}

func (r *BentoDeploymentReconciler) getYataiClient(ctx context.Context) (yataiClient **yataiclient.YataiClient, clusterName *string, err error) {
	restConfig := config.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		err = errors.Wrapf(err, "create kubernetes clientset")
		return
	}
	var yataiConf *commonconfig.YataiConfig

	if cachedYataiConf != nil {
		yataiConf = cachedYataiConf
	} else {
		yataiConf, err = commonconfig.GetYataiConfig(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
			secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
			return secret, errors.Wrap(err, "get secret")
		}, commonconsts.YataiDeploymentComponentName, false)
		isNotFound := k8serrors.IsNotFound(err)
		if err != nil && !isNotFound {
			err = errors.Wrap(err, "get yatai config")
			return
		}

		if isNotFound {
			return
		}
		cachedYataiConf = yataiConf
	}

	yataiEndpoint := yataiConf.Endpoint
	yataiAPIToken := yataiConf.ApiToken
	if yataiEndpoint == "" {
		return
	}

	clusterName_ := yataiConf.ClusterName
	if clusterName_ == "" {
		clusterName_ = DefaultClusterName
	}
	yataiClient_ := yataiclient.NewYataiClient(yataiEndpoint, fmt.Sprintf("%s:%s:%s", commonconsts.YataiDeploymentComponentName, clusterName_, yataiAPIToken))
	yataiClient = &yataiClient_
	clusterName = &clusterName_
	return
}

func (r *BentoDeploymentReconciler) getKubeName(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string, debug bool) string {
	if runnerName != nil && bento.Spec.Runners != nil {
		for idx, runner := range bento.Spec.Runners {
			if runner.Name == *runnerName {
				if debug {
					return fmt.Sprintf("%s-runner-d-%d", bentoDeployment.Name, idx)
				}
				return fmt.Sprintf("%s-runner-%d", bentoDeployment.Name, idx)
			}
		}
	}
	if debug {
		return fmt.Sprintf("%s-d", bentoDeployment.Name)
	}
	return bentoDeployment.Name
}

func (r *BentoDeploymentReconciler) getKubeLabels(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string) map[string]string {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(bento.Spec.Tag, ":")
	labels := map[string]string{
		commonconsts.KubeLabelYataiBentoDeployment:           bentoDeployment.Name,
		commonconsts.KubeLabelBentoRepository:                bentoRepositoryName,
		commonconsts.KubeLabelBentoVersion:                   bentoVersion,
		commonconsts.KubeLabelYataiBentoDeploymentTargetType: DeploymentTargetTypeProduction,
		commonconsts.KubeLabelCreator:                        "yatai-deployment",
	}
	if runnerName != nil {
		labels[commonconsts.KubeLabelYataiBentoDeploymentComponentType] = commonconsts.YataiBentoDeploymentComponentRunner
		labels[commonconsts.KubeLabelYataiBentoDeploymentComponentName] = *runnerName
	} else {
		labels[commonconsts.KubeLabelYataiBentoDeploymentComponentType] = commonconsts.YataiBentoDeploymentComponentApiServer
	}
	extraLabels := bentoDeployment.Spec.Labels
	if runnerName != nil {
		for _, runner := range bentoDeployment.Spec.Runners {
			if runner.Name != *runnerName {
				continue
			}
			extraLabels = runner.Labels
			break
		}
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return labels
}

func (r *BentoDeploymentReconciler) getKubeAnnotations(bentoDeployment *servingv2alpha1.BentoDeployment, bento *resourcesv1alpha1.Bento, runnerName *string) map[string]string {
	bentoRepositoryName, bentoVersion := getBentoRepositoryNameAndBentoVersion(bento)
	annotations := map[string]string{
		commonconsts.KubeAnnotationBentoRepository: bentoRepositoryName,
		commonconsts.KubeAnnotationBentoVersion:    bentoVersion,
	}
	var extraAnnotations map[string]string
	if bentoDeployment.Spec.ExtraPodMetadata != nil {
		extraAnnotations = bentoDeployment.Spec.ExtraPodMetadata.Annotations
	} else {
		extraAnnotations = map[string]string{}
	}
	if runnerName != nil {
		for _, runner := range bentoDeployment.Spec.Runners {
			if runner.Name != *runnerName {
				continue
			}
			if runner.ExtraPodMetadata != nil {
				extraAnnotations = runner.ExtraPodMetadata.Annotations
			}
			break
		}
	}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}
	return annotations
}

func (r *BentoDeploymentReconciler) doRegisterYataiComponent() (err error) {
	logs := log.Log.WithValues("func", "doRegisterYataiComponent")

	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute*5)
	defer cancel()

	logs.Info("getting yatai client")
	yataiClient, clusterName, err := r.getYataiClient(ctx)
	if err != nil {
		err = errors.Wrap(err, "get yatai client")
		return
	}

	if yataiClient == nil {
		logs.Info("yatai client is nil")
		return
	}

	yataiClient_ := *yataiClient

	restConfig := config.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		err = errors.Wrapf(err, "create kubernetes clientset")
		return
	}

	namespace, err := commonconfig.GetYataiDeploymentNamespace(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
		secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		return secret, errors.Wrap(err, "get secret")
	})
	if err != nil {
		err = errors.Wrap(err, "get yatai deployment namespace")
		return
	}

	_, err = yataiClient_.RegisterYataiComponent(ctx, *clusterName, &schemasv1.RegisterYataiComponentSchema{
		Name:          modelschemas.YataiComponentNameDeployment,
		KubeNamespace: namespace,
		Version:       version.Version,
		SelectorLabels: map[string]string{
			"app.kubernetes.io/name": "yatai-deployment",
		},
		Manifest: &modelschemas.YataiComponentManifestSchema{
			SelectorLabels: map[string]string{
				"app.kubernetes.io/name": "yatai-deployment",
			},
			LatestCRDVersion: "v2alpha1",
		},
	})

	return err
}

func (r *BentoDeploymentReconciler) registerYataiComponent() {
	logs := log.Log.WithValues("func", "registerYataiComponent")
	err := r.doRegisterYataiComponent()
	if err != nil {
		logs.Error(err, "registerYataiComponent")
	}
	ticker := time.NewTicker(time.Minute * 5)
	for range ticker.C {
		err := r.doRegisterYataiComponent()
		if err != nil {
			logs.Error(err, "registerYataiComponent")
		}
	}
}
