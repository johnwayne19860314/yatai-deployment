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
	"strings"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/bentoml/yatai-schemas/schemasv1"

	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-common/system"

	servingv2alpha1 "github.com/bentoml/yatai-deployment/apis/serving/v2alpha1"
)

func (r *BentoDeploymentReconciler) createOrUpdateIngresses(ctx context.Context, opt createOrUpdateIngressOption) (modified bool, err error) {
	logs := log.FromContext(ctx)

	bentoDeployment := opt.bentoDeployment
	bento := opt.bento

	ingresses, err := r.generateIngresses(ctx, generateIngressesOption{
		yataiClient:     opt.yataiClient,
		bentoDeployment: bentoDeployment,
		bento:           bento,
	})
	if err != nil {
		return
	}

	for _, ingress := range ingresses {
		logs := logs.WithValues("namespace", ingress.Namespace, "ingressName", ingress.Name)
		ingressNamespacedName := fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name)

		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetIngress", "Getting Ingress %s", ingressNamespacedName)

		oldIngress := &networkingv1.Ingress{}
		err = r.Get(ctx, types.NamespacedName{Name: ingress.Name, Namespace: ingress.Namespace}, oldIngress)
		oldIngressIsNotFound := k8serrors.IsNotFound(err)
		if err != nil && !oldIngressIsNotFound {
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "GetIngress", "Failed to get Ingress %s: %s", ingressNamespacedName, err)
			logs.Error(err, "Failed to get Ingress.")
			return
		}
		err = nil

		if oldIngressIsNotFound {
			if !bentoDeployment.Spec.Ingress.Enabled {
				logs.Info("Ingress not enabled. Skipping.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetIngress", "Skipping Ingress %s", ingressNamespacedName)
				continue
			}

			logs.Info("Ingress not found. Creating a new one.")

			err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(ingress), "set last applied annotation for ingress %s", ingress.Name)
			if err != nil {
				logs.Error(err, "Failed to set last applied annotation.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for Ingress %s: %s", ingressNamespacedName, err)
				return
			}

			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "CreateIngress", "Creating a new Ingress %s", ingressNamespacedName)
			err = r.Create(ctx, ingress)
			if err != nil {
				logs.Error(err, "Failed to create Ingress.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "CreateIngress", "Failed to create Ingress %s: %s", ingressNamespacedName, err)
				return
			}
			logs.Info("Ingress created.")
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "CreateIngress", "Created Ingress %s", ingressNamespacedName)
			modified = true
		} else {
			logs.Info("Ingress found.")

			if !bentoDeployment.Spec.Ingress.Enabled {
				logs.Info("Ingress not enabled. Deleting.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "DeleteIngress", "Deleting Ingress %s", ingressNamespacedName)
				err = r.Delete(ctx, ingress)
				if err != nil {
					logs.Error(err, "Failed to delete Ingress.")
					r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "DeleteIngress", "Failed to delete Ingress %s: %s", ingressNamespacedName, err)
					return
				}
				logs.Info("Ingress deleted.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "DeleteIngress", "Deleted Ingress %s", ingressNamespacedName)
				modified = true
				continue
			}

			// Keep host unchanged
			ingress.Spec.Rules[0].Host = oldIngress.Spec.Rules[0].Host

			var patchResult *patch.PatchResult
			patchResult, err = patch.DefaultPatchMaker.Calculate(oldIngress, ingress)
			if err != nil {
				logs.Error(err, "Failed to calculate patch.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "CalculatePatch", "Failed to calculate patch for Ingress %s: %s", ingressNamespacedName, err)
				return
			}

			if !patchResult.IsEmpty() {
				logs.Info("Ingress spec is different. Updating Ingress.")

				err = errors.Wrapf(patch.DefaultAnnotator.SetLastAppliedAnnotation(ingress), "set last applied annotation for ingress %s", ingress.Name)
				if err != nil {
					logs.Error(err, "Failed to set last applied annotation.")
					r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "SetLastAppliedAnnotation", "Failed to set last applied annotation for Ingress %s: %s", ingressNamespacedName, err)
					return
				}

				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateIngress", "Updating Ingress %s", ingressNamespacedName)
				err = r.Update(ctx, ingress)
				if err != nil {
					logs.Error(err, "Failed to update Ingress.")
					r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "UpdateIngress", "Failed to update Ingress %s: %s", ingressNamespacedName, err)
					return
				}
				logs.Info("Ingress updated.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateIngress", "Updated Ingress %s", ingressNamespacedName)
				modified = true
			} else {
				logs.Info("Ingress spec is the same. Skipping update.")
				r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "UpdateIngress", "Skipping update Ingress %s", ingressNamespacedName)
			}
		}
	}

	return
}

func (r *BentoDeploymentReconciler) generateIngressHost(ctx context.Context, bentoDeployment *servingv2alpha1.BentoDeployment) (string, error) {
	return r.generateDefaultHostname(ctx, bentoDeployment)
}

func (r *BentoDeploymentReconciler) generateDefaultHostname(ctx context.Context, bentoDeployment *servingv2alpha1.BentoDeployment) (string, error) {
	var domainSuffix string

	if cachedDomainSuffix != nil {
		domainSuffix = *cachedDomainSuffix
	} else {
		restConfig := config.GetConfigOrDie()
		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return "", errors.Wrapf(err, "create kubernetes clientset")
		}

		domainSuffix, err = system.GetDomainSuffix(ctx, func(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
			configmap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
			return configmap, errors.Wrap(err, "get configmap")
		}, clientset)
		if err != nil {
			return "", errors.Wrapf(err, "get domain suffix")
		}

		cachedDomainSuffix = &domainSuffix
	}
	return fmt.Sprintf("%s-%s.%s", bentoDeployment.Name, bentoDeployment.Namespace, domainSuffix), nil
}

func (r *BentoDeploymentReconciler) GetIngressConfig(ctx context.Context) (ingressConfig *IngressConfig, err error) {
	if cachedIngressConfig != nil {
		ingressConfig = cachedIngressConfig
		return
	}

	restConfig := config.GetConfigOrDie()
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		err = errors.Wrapf(err, "create kubernetes clientset")
		return
	}

	configMap, err := system.GetNetworkConfigConfigMap(ctx, func(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
		configmap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		return configmap, errors.Wrap(err, "get network config configmap")
	})
	if err != nil {
		err = errors.Wrapf(err, "failed to get configmap %s", commonconsts.KubeConfigMapNameNetworkConfig)
		return
	}

	var className *string

	className_ := strings.TrimSpace(configMap.Data[commonconsts.KubeConfigMapKeyNetworkConfigIngressClass])
	if className_ != "" {
		className = &className_
	}

	annotations := make(map[string]string)

	annotations_ := strings.TrimSpace(configMap.Data[commonconsts.KubeConfigMapKeyNetworkConfigIngressAnnotations])
	if annotations_ != "" {
		err = json.Unmarshal([]byte(annotations_), &annotations)
		if err != nil {
			err = errors.Wrapf(err, "failed to json unmarshal %s in configmap %s: %s", commonconsts.KubeConfigMapKeyNetworkConfigIngressAnnotations, commonconsts.KubeConfigMapNameNetworkConfig, annotations_)
			return
		}
	}

	path := strings.TrimSpace(configMap.Data["ingress-path"])
	if path == "" {
		path = "/"
	}

	pathType := networkingv1.PathTypeImplementationSpecific

	pathType_ := strings.TrimSpace(configMap.Data["ingress-path-type"])
	if pathType_ != "" {
		pathType = networkingv1.PathType(pathType_)
	}

	ingressConfig = &IngressConfig{
		ClassName:   className,
		Annotations: annotations,
		Path:        path,
		PathType:    pathType,
	}

	cachedIngressConfig = ingressConfig

	return
}

func (r *BentoDeploymentReconciler) generateIngresses(ctx context.Context, opt generateIngressesOption) (ingresses []*networkingv1.Ingress, err error) {
	bentoRepositoryName, bentoVersion := getBentoRepositoryNameAndBentoVersion(opt.bento)
	bentoDeployment := opt.bentoDeployment
	bento := opt.bento

	kubeName := r.getKubeName(bentoDeployment, bento, nil, false)

	r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GenerateIngressHost", "Generating hostname for ingress")
	internalHost, err := r.generateIngressHost(ctx, bentoDeployment)
	if err != nil {
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "GenerateIngressHost", "Failed to generate hostname for ingress: %v", err)
		return
	}
	r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GenerateIngressHost", "Generated hostname for ingress: %s", internalHost)

	annotations := r.getKubeAnnotations(bentoDeployment, bento, nil)

	tag := fmt.Sprintf("%s:%s", bentoRepositoryName, bentoVersion)
	orgName := "unknown"

	if opt.yataiClient != nil {
		yataiClient := *opt.yataiClient
		var organization *schemasv1.OrganizationFullSchema
		r.Recorder.Eventf(bentoDeployment, corev1.EventTypeNormal, "GetOrganization", "Getting organization for bento %s", tag)
		organization, err = yataiClient.GetOrganization(ctx)
		if err != nil {
			r.Recorder.Eventf(bentoDeployment, corev1.EventTypeWarning, "GetOrganization", "Failed to get organization: %v", err)
			return
		}
		orgName = organization.Name
	}

	annotations["nginx.ingress.kubernetes.io/configuration-snippet"] = fmt.Sprintf(`
more_set_headers "X-Powered-By: Yatai";
more_set_headers "X-Yatai-Org-Name: %s";
more_set_headers "X-Yatai-Bento: %s";
`, orgName, tag)

	annotations["nginx.ingress.kubernetes.io/ssl-redirect"] = "false"

	labels := r.getKubeLabels(bentoDeployment, bento, nil)

	kubeNs := bentoDeployment.Namespace

	ingressConfig, err := r.GetIngressConfig(ctx)
	if err != nil {
		err = errors.Wrapf(err, "get ingress config")
		return
	}

	ingressClassName := ingressConfig.ClassName
	ingressAnnotations := ingressConfig.Annotations
	ingressPath := ingressConfig.Path
	ingressPathType := ingressConfig.PathType

	for k, v := range ingressAnnotations {
		annotations[k] = v
	}

	for k, v := range opt.bentoDeployment.Spec.Ingress.Annotations {
		annotations[k] = v
	}

	for k, v := range opt.bentoDeployment.Spec.Ingress.Labels {
		labels[k] = v
	}

	var tls []networkingv1.IngressTLS

	if opt.bentoDeployment.Spec.Ingress.TLS != nil && opt.bentoDeployment.Spec.Ingress.TLS.SecretName != "" {
		tls = make([]networkingv1.IngressTLS, 0, 1)
		tls = append(tls, networkingv1.IngressTLS{
			Hosts:      []string{internalHost},
			SecretName: opt.bentoDeployment.Spec.Ingress.TLS.SecretName,
		})
	}

	serviceName := r.getGenericServiceName(bentoDeployment, bento, nil)

	interIng := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        kubeName,
			Namespace:   kubeNs,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ingressClassName,
			TLS:              tls,
			Rules: []networkingv1.IngressRule{
				{
					Host: internalHost,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     ingressPath,
									PathType: &ingressPathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: serviceName,
											Port: networkingv1.ServiceBackendPort{
												Name: commonconsts.BentoServicePortName,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err = ctrl.SetControllerReference(bentoDeployment, interIng, r.Scheme)
	if err != nil {
		err = errors.Wrapf(err, "set ingress %s controller reference", interIng.Name)
		return
	}

	ings := []*networkingv1.Ingress{interIng}

	return ings, err
}
