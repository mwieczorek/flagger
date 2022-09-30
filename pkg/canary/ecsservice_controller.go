/*
Copyright 2020 The Flux authors

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

package canary

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	flaggerv1 "github.com/fluxcd/flagger/pkg/apis/flagger/v1beta1"
	clientset "github.com/fluxcd/flagger/pkg/client/clientset/versioned"

	ecs "github.com/getndazn/mwieczorek-temp/dazn-gateway/ecs-controller/apis/v1alpha1"
	sd "github.com/getndazn/mwieczorek-temp/dazn-gateway/servicediscovery-controller/apis/v1alpha1"
)

// EcsServiceController is managing the operations for Kubernetes EcsService kind
type EcsServiceController struct {
	kubeClient         kubernetes.Interface
	dynamicClient      dynamic.Interface
	flaggerClient      clientset.Interface
	logger             *zap.SugaredLogger
	configTracker      Tracker
	labels             []string
	includeLabelPrefix []string
}

// Initialize creates the primary deployment, hpa,
// scales to zero the canary deployment and returns the pod selector label and container ports
func (c *EcsServiceController) Initialize(cd *flaggerv1.Canary) (err error) {
	if err := c.createPrimaryEcsService(cd, c.includeLabelPrefix); err != nil {
		return fmt.Errorf("createPrimaryDeployment failed: %w", err)
	}

	if cd.Status.Phase == "" || cd.Status.Phase == flaggerv1.CanaryPhaseInitializing {
		if !cd.SkipAnalysis() {
			if err := c.IsPrimaryReady(cd); err != nil {
				return fmt.Errorf("%w", err)
			}
		}

		c.logger.With("canary", fmt.Sprintf("%s.%s", cd.Name, cd.Namespace)).
			Infof("Scaling down Deployment %s.%s", cd.Spec.TargetRef.Name, cd.Namespace)
		if err := c.ScaleToZero(cd); err != nil {
			return fmt.Errorf("scaling down canary deployment %s.%s failed: %w", cd.Spec.TargetRef.Name, cd.Namespace, err)
		}
	}

	return nil
}

// Promote copies the pod spec, secrets and config maps from canary to primary
func (c *EcsServiceController) Promote(cd *flaggerv1.Canary) error {
	targetName := cd.Spec.TargetRef.Name
	primaryName := fmt.Sprintf("%s-primary", targetName)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		canary, err := c.getEcsService(cd.Namespace, targetName)
		if err != nil {
			return fmt.Errorf("ecsservice %s.%s get query error: %w", targetName, cd.Namespace, err)
		}

		primary, err := c.getEcsService(cd.Namespace, primaryName)
		if err != nil {
			return fmt.Errorf("ecsservice %s.%s get query error: %w", primaryName, cd.Namespace, err)
		}

		primaryCopy := primary.DeepCopy()
		primaryCopy.Spec.CapacityProviderStrategy = canary.Spec.CapacityProviderStrategy
		primaryCopy.Spec.DeploymentConfiguration = canary.Spec.DeploymentConfiguration
		primaryCopy.Spec.EnableECSManagedTags = canary.Spec.EnableECSManagedTags
		primaryCopy.Spec.TaskDefinition = canary.Spec.TaskDefinition

		// update deploy annotations
		primaryCopy.ObjectMeta.Annotations = make(map[string]string)
		filteredAnnotations := includeLabelsByPrefix(canary.ObjectMeta.Annotations, c.includeLabelPrefix)
		for k, v := range filteredAnnotations {
			primaryCopy.ObjectMeta.Annotations[k] = v
		}
		// update deploy labels
		primaryCopy.ObjectMeta.Labels = make(map[string]string)
		filteredLabels := includeLabelsByPrefix(canary.ObjectMeta.Labels, c.includeLabelPrefix)
		for k, v := range filteredLabels {
			primaryCopy.ObjectMeta.Labels[k] = v
		}

		unstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(primaryCopy)
		if err != nil {
			return fmt.Errorf("converting ecsservice to unstructured %s.%s failed: %w", primaryCopy.GetName(), cd.Namespace, err)
		}
		un := &unstructured.Unstructured{Object: unstr}

		_, err = c.getEcsServiceResource().Namespace(cd.Namespace).Update(context.TODO(), un, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("updating ecsservice %s.%s  spec failed: %w",
			primaryName, cd.Namespace, err)
	}

	return nil
}

// HasTargetChanged returns true if the canary deployment pod spec has changed
func (c *EcsServiceController) HasTargetChanged(cd *flaggerv1.Canary) (bool, error) {
	targetName := cd.Spec.TargetRef.Name
	ecssvc, err := c.getEcsService(cd.Namespace, targetName)
	if err != nil {
		return false, fmt.Errorf("ecsservice %s.%s get query error: %w", targetName, cd.Namespace, err)
	}

	return hasSpecChanged(cd, ecssvc.Spec)
}

// ScaleToZero Scale sets the canary deployment replicas
func (c *EcsServiceController) ScaleToZero(cd *flaggerv1.Canary) error {
	targetName := cd.Spec.TargetRef.Name

	ecssvc, err := c.getEcsService(cd.Namespace, targetName)
	if err != nil {
		return fmt.Errorf("ecsservice %s.%s get query error: %w", targetName, cd.Namespace, err)
	}

	depCopy := ecssvc.DeepCopy()
	depCopy.Spec.DesiredCount = int64p(0)

	unstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(depCopy)
	if err != nil {
		return fmt.Errorf("converting ecsservice to unstructured %s.%s failed: %w", depCopy.GetName(), cd.Namespace, err)
	}
	un := &unstructured.Unstructured{Object: unstr}

	_, err = c.getEcsServiceResource().Namespace(cd.Namespace).Update(context.TODO(), un, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("creating ecsservice %s.%s failed: %w", depCopy.GetName(), cd.Namespace, err)
	}

	return nil
}

func (c *EcsServiceController) ScaleFromZero(cd *flaggerv1.Canary) error {
	targetName := cd.Spec.TargetRef.Name
	ecssvc, err := c.getEcsService(cd.Namespace, targetName)
	if err != nil {
		return fmt.Errorf("ecsservice %s.%s get query error: %w", targetName, cd.Namespace, err)
	}

	replicas := ecssvc.Spec.DesiredCount

	ecssvcCopy := ecssvc.DeepCopy()
	ecssvcCopy.Spec.DesiredCount = replicas

	unstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ecssvcCopy)
	if err != nil {
		return fmt.Errorf("converting ecsservice to unstructured %s.%s failed: %w", ecssvcCopy.GetName(), cd.Namespace, err)
	}
	un := &unstructured.Unstructured{Object: unstr}

	_, err = c.getEcsServiceResource().Namespace(cd.Namespace).Update(context.TODO(), un, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("creating ecsservice %s.%s failed: %w", ecssvcCopy.GetName(), cd.Namespace, err)
	}

	if err != nil {
		return fmt.Errorf("scaling up %s.%s to %v failed: %v", ecssvcCopy.GetName(), ecssvcCopy.Namespace, replicas, err)
	}
	return nil
}

// GetMetadata returns the pod label selector and svc ports
func (c *EcsServiceController) GetMetadata(cd *flaggerv1.Canary) (string, string, map[string]int32, error) {
	ports := map[string]int32{"": 0}
	return "", "", ports, nil
}

func (c *EcsServiceController) getEcsServiceResource() dynamic.NamespaceableResourceInterface {
	ecsServiceRes := schema.GroupVersionResource{Group: "ecs.services.k8s.aws", Version: "v1alpha1", Resource: "services"}
	return c.dynamicClient.Resource(ecsServiceRes)
}

func (c *EcsServiceController) getSDServiceResource() dynamic.NamespaceableResourceInterface {
	sdServiceRes := schema.GroupVersionResource{Group: "servicediscovery.services.k8s.aws", Version: "v1alpha1", Resource: "services"}
	return c.dynamicClient.Resource(sdServiceRes)
}

func (c *EcsServiceController) getEcsService(namespace, name string) (*ecs.Service, error) {
	s, err := c.getEcsServiceResource().Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	unst := s.UnstructuredContent()
	ecsservice := ecs.Service{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unst, &ecsservice)
	if err != nil {
		return nil, fmt.Errorf("ecssercice %s.%s convert from unstructured error: %w", name, namespace, err)
	}

	return &ecsservice, nil
}

func (c *EcsServiceController) getSDService(namespace, name string) (*sd.Service, error) {
	s, err := c.getSDServiceResource().Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	unst := s.UnstructuredContent()
	sdservice := sd.Service{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unst, &sdservice)
	if err != nil {
		return nil, fmt.Errorf("sdsercice %s.%s convert from unstructured error: %w", name, namespace, err)
	}

	return &sdservice, nil
}

func (c *EcsServiceController) getSDServiceByIdLabel(namespace, idLabelValue string) (*sd.Service, error) {
	selector := metav1.LabelSelector{MatchLabels: map[string]string{"cloudmap-service-id": idLabelValue}}

	sList, err := c.getSDServiceResource().Namespace(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(selector.MatchLabels).String()})
	if err != nil {
		return nil, err
	}
	if len(sList.Items) != 1 {
		return nil, errors.NewNotFound(schema.GroupResource{Group: "servicediscovery.services.k8s.aws", Resource: "service"}, idLabelValue)
	}
	unst := sList.Items[0].UnstructuredContent()
	sdservice := sd.Service{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unst, &sdservice)
	if err != nil {
		return nil, fmt.Errorf("sdsercice convert from unstructured error: %w", err)
	}
	return &sdservice, nil
}

func getSdID(s *ecs.Service) (string, error) {
	if len(s.Spec.ServiceRegistries) != 1 {
		return "", fmt.Errorf("ecssercice has no cloudmap service registires configured")
	}
	a, err := arn.Parse(*s.Spec.ServiceRegistries[0].RegistryARN)
	if err != nil {
		return "", fmt.Errorf("parse arn: %w", err)
	}

	return strings.TrimPrefix(a.Resource, "service/"), nil
}

func (c *EcsServiceController) createPrimaryEcsService(cd *flaggerv1.Canary, includeLabelPrefix []string) error {
	targetName := cd.Spec.TargetRef.Name
	primaryName := fmt.Sprintf("%s-primary", cd.Spec.TargetRef.Name)

	canarySvc, err := c.getEcsService(cd.Namespace, targetName)
	if err != nil {
		return fmt.Errorf("ecssercice %s.%s get query error: %w", targetName, cd.Namespace, err)
	}

	cmSvcID, err := getSdID(canarySvc)
	if err != nil {
		return fmt.Errorf("ecssercice %s.%s get query error: %w", targetName, cd.Namespace, err)
	}

	canarySd, err := c.getSDServiceByIdLabel(cd.Namespace, cmSvcID)
	if err != nil {
		return fmt.Errorf("cloudmap svc in %s NS, label: %s get query error: %w", cd.Namespace, cmSvcID, err)
	}

	sdPrimaryName := fmt.Sprintf("%s-primary", canarySd.Name)

	_, err = c.getSDService(cd.Namespace, sdPrimaryName)
	if errors.IsNotFound(err) {
		primarySd := canarySd.DeepCopy()
		primarySd.SetName(sdPrimaryName)
		primarySd.SetOwnerReferences([]metav1.OwnerReference{
			*metav1.NewControllerRef(cd, schema.GroupVersionKind{
				Group:   flaggerv1.SchemeGroupVersion.Group,
				Version: flaggerv1.SchemeGroupVersion.Version,
				Kind:    flaggerv1.CanaryKind,
			}),
		})
		primarySd.SetResourceVersion("")
		primarySd.SetAnnotations(filterSdAdoptedAnnotation(primarySd))
		primarySd.Spec.Tags = filterSdMirrorTag(primarySd)
		primarySd.Spec.Name = aws.String(sdPrimaryName)
		primarySd.Status = sd.ServiceStatus{}
		primarySd.Labels = nil

		unstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(primarySd)
		if err != nil {
			return fmt.Errorf("converting cloudmapservice to unstructured %s.%s failed: %w", primarySd.GetName(), cd.Namespace, err)
		}
		un := &unstructured.Unstructured{Object: unstr}

		_, err = c.getSDServiceResource().Namespace(cd.Namespace).Create(context.TODO(), un, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating cloudmap service %s.%s failed: %w", primarySd.GetName(), cd.Namespace, err)
		}

		c.logger.With("canary", fmt.Sprintf("%s.%s", cd.Name, cd.Namespace)).
			Infof("CloudmapService %s.%s created", primarySd.GetName(), cd.Namespace)
	}

	primarySd, err := c.isSdServiceReady(cd.Namespace, sdPrimaryName)
	if err != nil {
		return err
	}

	_, err = c.getEcsService(cd.Namespace, primaryName)
	if errors.IsNotFound(err) {
		primarySvc := canarySvc.DeepCopy()

		primarySvc.SetName(primaryName)
		primarySvc.SetOwnerReferences([]metav1.OwnerReference{
			*metav1.NewControllerRef(cd, schema.GroupVersionKind{
				Group:   flaggerv1.SchemeGroupVersion.Group,
				Version: flaggerv1.SchemeGroupVersion.Version,
				Kind:    flaggerv1.CanaryKind,
			}),
		})
		primarySvc.SetResourceVersion("")
		primarySvc.SetAnnotations(filterAdoptedAnnotation(primarySvc))
		primarySvc.Spec.Tags = filterMirrorTag(primarySvc)
		primarySvc.Spec.Tags = append(primarySvc.Spec.Tags, &ecs.Tag{
			Key:   aws.String("com.dazn.ecs-controller.ecsservice.primary"),
			Value: aws.String("true"),
		})
		primarySvc.Spec.ServiceName = aws.String(primaryName)

		primarySvc.Spec.ServiceRegistries = []*ecs.ServiceRegistry{{RegistryARN: aws.String(string(*primarySd.Status.ACKResourceMetadata.ARN))}}

		unstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(primarySvc)
		if err != nil {
			return fmt.Errorf("converting ecsservice to unstructured %s.%s failed: %w", primarySvc.GetName(), cd.Namespace, err)
		}
		un := &unstructured.Unstructured{Object: unstr}

		_, err = c.getEcsServiceResource().Namespace(cd.Namespace).Create(context.TODO(), un, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating ecsservice %s.%s failed: %w", primarySvc.GetName(), cd.Namespace, err)
		}

		c.logger.With("canary", fmt.Sprintf("%s.%s", cd.Name, cd.Namespace)).
			Infof("EcsService %s.%s created", primarySvc.GetName(), cd.Namespace)
	}

	return nil
}

func (c *EcsServiceController) isSdServiceReady(namespace, name string) (*sd.Service, error) {
	s, err := c.getSDService(namespace, name)
	if err != nil {
		return nil, err
	}

	if s.Status.ID == nil {
		return nil, fmt.Errorf("cloudmap service %s.%s not provisioned yet", namespace, name)
	}

	return s, nil
}

func filterMirrorTag(c *ecs.Service) []*ecs.Tag {
	var result []*ecs.Tag
	for _, i := range c.Spec.Tags {
		if *i.Key != "com.dazn.ecs-controller.ecsservice.mirror" {
			result = append(result, i)
		}
	}
	return result
}

func filterSdMirrorTag(c *sd.Service) []*sd.Tag {
	var result []*sd.Tag
	for _, i := range c.Spec.Tags {
		if *i.Key != "com.dazn.servicediscovery-controller.service.mirror" {
			result = append(result, i)
		}
	}
	return result
}

func filterAdoptedAnnotation(c *ecs.Service) map[string]string {
	result := map[string]string{}
	for k, v := range c.GetAnnotations() {
		if k != "services.k8s.aws/adopted" {
			result[k] = v
		}
	}
	return result
}

func filterSdAdoptedAnnotation(c *sd.Service) map[string]string {
	result := map[string]string{}
	for k, v := range c.GetAnnotations() {
		if k != "services.k8s.aws/adopted" {
			result[k] = v
		}
	}
	return result
}

func (c *EcsServiceController) HaveDependenciesChanged(cd *flaggerv1.Canary) (bool, error) {
	return c.configTracker.HasConfigChanged(cd)
}

// Finalize will set the replica count from the primary to the reference instance.  This method is used
// during a delete to attempt to revert the deployment back to the original state.  Error is returned if unable
// update the reference deployment replicas to the primary replicas
func (c *EcsServiceController) Finalize(cd *flaggerv1.Canary) error {

	// get ref deployment
	refDep, err := c.kubeClient.AppsV1().Deployments(cd.Namespace).Get(context.TODO(), cd.Spec.TargetRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("deplyoment %s.%s get query error: %w", cd.Spec.TargetRef.Name, cd.Namespace, err)
	}

	// get primary if possible, if not scale from zero
	primaryName := fmt.Sprintf("%s-primary", cd.Spec.TargetRef.Name)
	primaryDep, err := c.kubeClient.AppsV1().Deployments(cd.Namespace).Get(context.TODO(), primaryName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			if err := c.ScaleFromZero(cd); err != nil {
				return fmt.Errorf("ScaleFromZero failed: %w", err)
			}
			return nil
		}
		return fmt.Errorf("deplyoment %s.%s get query error: %w", primaryName, cd.Namespace, err)
	}

	// if both ref and primary present update the replicas of the ref to match the primary
	if refDep.Spec.Replicas != primaryDep.Spec.Replicas {
		// set the replicas value on the original reference deployment
		if err := c.scale(cd, int32Default(primaryDep.Spec.Replicas)); err != nil {
			return fmt.Errorf("scale failed: %w", err)
		}
	}
	return nil
}

// Scale sets the canary deployment replicas
func (c *EcsServiceController) scale(cd *flaggerv1.Canary, replicas int32) error {
	targetName := cd.Spec.TargetRef.Name
	dep, err := c.kubeClient.AppsV1().Deployments(cd.Namespace).Get(context.TODO(), targetName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("deployment %s.%s query error: %w", targetName, cd.Namespace, err)
	}

	depCopy := dep.DeepCopy()
	depCopy.Spec.Replicas = int32p(replicas)
	_, err = c.kubeClient.AppsV1().Deployments(dep.Namespace).Update(context.TODO(), depCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("scaling %s.%s to %v failed: %w", depCopy.GetName(), depCopy.Namespace, replicas, err)
	}
	return nil
}

// SyncStatus encodes the canary pod spec and updates the canary status
func (c *EcsServiceController) SyncStatus(cd *flaggerv1.Canary, status flaggerv1.CanaryStatus) error {
	ecssvc, err := c.getEcsService(cd.Namespace, cd.Spec.TargetRef.Name)
	if err != nil {
		return fmt.Errorf("ecsservice %s.%s get query error: %w", cd.Spec.TargetRef.Name, cd.Namespace, err)
	}

	configs, err := c.configTracker.GetConfigRefs(cd)
	if err != nil {
		return fmt.Errorf("GetConfigRefs failed: %w", err)
	}

	return syncCanaryStatus(c.flaggerClient, cd, status, ecssvc.Spec, func(cdCopy *flaggerv1.Canary) {
		cdCopy.Status.TrackedConfigs = configs
	})
}

// SetStatusFailedChecks updates the canary failed checks counter
func (c *EcsServiceController) SetStatusFailedChecks(cd *flaggerv1.Canary, val int) error {
	return setStatusFailedChecks(c.flaggerClient, cd, val)
}

// SetStatusWeight updates the canary status weight value
func (c *EcsServiceController) SetStatusWeight(cd *flaggerv1.Canary, val int) error {
	return setStatusWeight(c.flaggerClient, cd, val)
}

// SetStatusIterations updates the canary status iterations value
func (c *EcsServiceController) SetStatusIterations(cd *flaggerv1.Canary, val int) error {
	return setStatusIterations(c.flaggerClient, cd, val)
}

// SetStatusPhase updates the canary status phase
func (c *EcsServiceController) SetStatusPhase(cd *flaggerv1.Canary, phase flaggerv1.CanaryPhase) error {
	return setStatusPhase(c.flaggerClient, cd, phase)
}

// IsPrimaryReady checks the primary ecsservice status and returns an error if
// the ecsservice is in the middle of a rolling update or if the pods are unhealthy
// it will return a non retryable error if the rolling update is stuck
func (c *EcsServiceController) IsPrimaryReady(cd *flaggerv1.Canary) error {
	primaryName := fmt.Sprintf("%s-primary", cd.Spec.TargetRef.Name)
	primary, err := c.getEcsService(cd.Namespace, primaryName)
	if err != nil {
		return fmt.Errorf("ecsservice %s.%s get query error: %w", primaryName, cd.Namespace, err)
	}

	_, err = c.isEcsServiceReady(primary, cd.GetProgressDeadlineSeconds(), cd.GetAnalysisPrimaryReadyThreshold())
	if err != nil {
		return fmt.Errorf("%s.%s not ready: %w", primaryName, cd.Namespace, err)
	}

	if primary.Spec.DesiredCount == int64p(0) {
		return fmt.Errorf("halt %s.%s advancement: primary ecsservice is scaled to zero",
			cd.Name, cd.Namespace)
	}
	return nil
}

// IsCanaryReady checks the canary ecsservice status and returns an error if
// the ecsservice is in the middle of a rolling update or if the pods are unhealthy
// it will return a non retriable error if the rolling update is stuck
func (c *EcsServiceController) IsCanaryReady(cd *flaggerv1.Canary) (bool, error) {
	targetName := cd.Spec.TargetRef.Name
	canary, err := c.getEcsService(cd.Namespace, targetName)
	if err != nil {
		return true, fmt.Errorf("ecsservice %s.%s get query error: %w", targetName, cd.Namespace, err)
	}

	retryable, err := c.isEcsServiceReady(canary, cd.GetProgressDeadlineSeconds(), cd.GetAnalysisCanaryReadyThreshold())
	if err != nil {
		return retryable, fmt.Errorf(
			"canary deployment %s.%s not ready: %w",
			targetName, cd.Namespace, err,
		)
	}
	return true, nil
}

// isEcsServiceReady determines if a deployment is ready by checking the status conditions
// if a deployment has exceeded the progress deadline it returns a non retriable error
func (c *EcsServiceController) isEcsServiceReady(ecsservice *ecs.Service, deadline int, readyThreshold int) (bool, error) {
	if ecsservice.Status.Status == nil {
		return true, fmt.Errorf("no status in ecsservice: %s", ecsservice.GetName())
	}
	if ecsservice.Status.PendingCount != nil && *ecsservice.Status.PendingCount != 0 {
		return true, fmt.Errorf("waiting for pending tasks: %d", *ecsservice.Status.PendingCount)
	}

	if *ecsservice.Status.Status == "INACTIVE" {
		return false, fmt.Errorf("ecsservice status is INACTIVE")
	}

	for _, d := range ecsservice.Status.Deployments {
		if *d.RolloutState == string(ecs.DeploymentRolloutState_IN_PROGRESS) {
			return true, fmt.Errorf("waiting for deployment rollout: %s, state: %s", *d.ID, *d.RolloutState)
		}
		if *d.RolloutState == string(ecs.DeploymentRolloutState_FAILED) {
			return false, fmt.Errorf("cannot continue because of failure in ECS deployment: %s, state: %s", *d.ID, *d.RolloutState)
		}
	}
	return true, nil
}
