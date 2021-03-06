package ingress

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/appscode/go/errors"
	"github.com/appscode/go/log"
	"github.com/appscode/go/types"
	tools "github.com/appscode/kube-mon"
	"github.com/appscode/kube-mon/agents"
	"github.com/appscode/kutil"
	"github.com/appscode/kutil/meta"
	"github.com/appscode/kutil/tools/analytics"
	api "github.com/appscode/voyager/apis/voyager/v1beta1"
	cs "github.com/appscode/voyager/client/typed/voyager/v1beta1"
	"github.com/appscode/voyager/pkg/config"
	"github.com/appscode/voyager/pkg/eventer"
	"github.com/appscode/voyager/third_party/forked/cloudprovider"
	_ "github.com/appscode/voyager/third_party/forked/cloudprovider/providers"
	fakecloudprovider "github.com/appscode/voyager/third_party/forked/cloudprovider/providers/fake"
	pcm "github.com/coreos/prometheus-operator/pkg/client/monitoring/v1"
	apps "k8s.io/api/apps/v1beta1"
	core "k8s.io/api/core/v1"
	kext_cs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	core_listers "k8s.io/client-go/listers/core/v1"
)

type nodePortController struct {
	*controller
	CloudManager cloudprovider.Interface
}

var _ Controller = &nodePortController{}

func NewNodePortController(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	crdClient kext_cs.ApiextensionsV1beta1Interface,
	extClient cs.VoyagerV1beta1Interface,
	promClient pcm.MonitoringV1Interface,
	serviceLister core_listers.ServiceLister,
	endpointsLister core_listers.EndpointsLister,
	opt config.Options,
	ingress *api.Ingress) Controller {
	c := &nodePortController{
		controller: &controller{
			logger:          log.New(ctx),
			KubeClient:      kubeClient,
			CRDClient:       crdClient,
			VoyagerClient:   extClient,
			PromClient:      promClient,
			ServiceLister:   serviceLister,
			EndpointsLister: endpointsLister,
			Opt:             opt,
			Ingress:         ingress,
			recorder:        eventer.NewEventRecorder(kubeClient, "voyager operator"),
		},
	}
	c.logger.Infoln("Initializing cloud manager for provider", opt.CloudProvider)
	if opt.CloudProvider == "aws" || opt.CloudProvider == "gce" || opt.CloudProvider == "azure" {
		cloudInterface, err := cloudprovider.InitCloudProvider(opt.CloudProvider, opt.CloudConfigFile)
		if err != nil {
			c.logger.Errorln("Failed to initialize cloud provider:"+opt.CloudProvider, err)
		} else {
			c.logger.Infoln("Initialized cloud provider: "+opt.CloudProvider, cloudInterface)
			c.CloudManager = cloudInterface
		}
	} else if opt.CloudProvider == "gke" {
		cloudInterface, err := cloudprovider.InitCloudProvider("gce", opt.CloudConfigFile)
		if err != nil {
			c.logger.Errorln("Failed to initialize cloud provider:"+opt.CloudProvider, err)
		} else {
			c.logger.Infoln("Initialized cloud provider: "+opt.CloudProvider, cloudInterface)
			c.CloudManager = cloudInterface
		}
	} else if opt.CloudProvider == "acs" {
		cloudInterface, err := cloudprovider.InitCloudProvider("azure", opt.CloudConfigFile)
		if err != nil {
			c.logger.Errorln("Failed to initialize cloud provider:"+opt.CloudProvider, err)
		} else {
			c.logger.Infoln("Initialized cloud provider: "+opt.CloudProvider, cloudInterface)
			c.CloudManager = cloudInterface
		}
	} else if opt.CloudProvider == "minikube" {
		c.CloudManager = &fakecloudprovider.FakeCloud{}
	} else {
		c.logger.Infoln("No cloud manager found for provider", opt.CloudProvider)
	}
	return c
}

func (c *nodePortController) IsExists() bool {
	_, err := c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return false
	}
	_, err = c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return false
	}
	_, err = c.KubeClient.CoreV1().ConfigMaps(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		return false
	}
	if c.Opt.EnableRBAC {
		_, err = c.KubeClient.CoreV1().ServiceAccounts(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			return false
		}
		_, err = c.KubeClient.RbacV1beta1().Roles(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			return false
		}
		_, err = c.KubeClient.RbacV1beta1().RoleBindings(c.Ingress.Namespace).Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			return false
		}
	}
	return true
}

func (c *nodePortController) Create() error {
	// Service is created first so that NodePorts can be used in the haproxy.conf
	svc, err := c.ensureService(nil)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressServiceCreateFailed,
			"Failed to create NodePortService, Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	if err := c.waitForNodePortAssignment(); err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressServiceCreateFailed,
			"Timeout waiting for NodePort assignment, %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	err = c.EnsureFirewall(svc)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressFirewallUpdateFailed,
			"Failed to ensure firewall, %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}

	err = c.generateConfig()
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressHAProxyConfigCreateFailed,
			"Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	err = c.ensureConfigMap()
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressConfigMapCreateFailed,
			"Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonIngressConfigMapCreateSuccessful,
		"Successfully created ConfigMap %s",
		c.Ingress.OffshootName(),
	)

	// If RBAC is enabled we need to ensure service account
	if c.Opt.EnableRBAC {
		err := c.ensureRBAC()
		if err != nil {
			c.recorder.Event(
				c.Ingress.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonIngressRBACFailed,
				err.Error(),
			)
			return errors.FromErr(err).Err()
		}
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonIngressRBACSuccessful,
			"Successfully applied RBAC",
		)
	}

	_, err = c.ensurePods(nil)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressControllerCreateFailed,
			"Failed to create NodePortPods, Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonIngressControllerCreateSuccessful,
		"Successfully created NodePortPods")

	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonIngressServiceCreateSuccessful,
		"Successfully created NodePortService",
	)

	if c.Ingress.Stats() {
		err := c.ensureStatsService()
		// Error ignored intentionally
		if err != nil {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonIngressStatsServiceCreateFailed,
				"Failed to create Stats Service. Reason: %s",
				err.Error(),
			)
		} else {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				core.EventTypeNormal,
				eventer.EventReasonIngressStatsServiceCreateSuccessful,
				"Successfully created Stats Service %s",
				c.Ingress.StatsServiceName(),
			)
		}
	}

	monSpec, err := tools.Parse(c.Ingress.Annotations, api.EngressKey, api.DefaultExporterPortNumber)
	if err != nil {
		return errors.FromErr(err).Err()
	}
	if monSpec != nil {
		agent := agents.New(monSpec.Agent, c.KubeClient, c.CRDClient, c.PromClient)
		vt, err := agent.CreateOrUpdate(c.Ingress.StatsAccessor(), monSpec)
		// Error Ignored intentionally
		if err != nil {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonIngressServiceMonitorCreateFailed,
				err.Error(),
			)
		} else if vt != kutil.VerbUnchanged {
			c.recorder.Eventf(
				c.Ingress.ObjectReference(),
				core.EventTypeNormal,
				eventer.EventReasonIngressServiceMonitorCreateSuccessful,
				"Successfully %s ServiceMonitor",
				vt,
			)
		}
	}

	return nil
}

func (c *nodePortController) Update(mode UpdateMode, old *api.Ingress) error {
	svc, err := c.ensureService(old)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressServiceUpdateFailed,
			"Failed to update LBService, %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	if err := c.waitForNodePortAssignment(); err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressServiceUpdateFailed,
			"Timeout waiting for NodePort assignment, %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	err = c.EnsureFirewall(svc)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressFirewallUpdateFailed,
			"Failed to ensure firewall, %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}

	err = c.generateConfig()
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressHAProxyConfigCreateFailed,
			"Reason: %s",
			err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	// Update HAProxy config
	err = c.updateConfigMap()
	if err != nil {
		return errors.FromErr(err).Err()
	}

	// If RBAC is enabled we need to ensure service account
	if c.Opt.EnableRBAC {
		err := c.ensureRBAC()
		if err != nil {
			c.recorder.Event(
				c.Ingress.ObjectReference(),
				core.EventTypeWarning,
				eventer.EventReasonIngressRBACFailed,
				err.Error(),
			)
			return errors.FromErr(err).Err()
		}
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeNormal,
			eventer.EventReasonIngressRBACSuccessful,
			"Successfully applied RBAC",
		)
	}

	_, err = c.ensurePods(old)
	if err != nil {
		c.recorder.Eventf(
			c.Ingress.ObjectReference(),
			core.EventTypeWarning,
			eventer.EventReasonIngressUpdateFailed,
			"Failed to update Pods, %s", err.Error(),
		)
		return errors.FromErr(err).Err()
	}
	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonIngressUpdateSuccessful,
		"Successfully updated Pods",
	)

	c.recorder.Eventf(
		c.Ingress.ObjectReference(),
		core.EventTypeNormal,
		eventer.EventReasonIngressServiceUpdateSuccessful,
		"Successfully updated NodePort Service",
	)

	if mode&UpdateStats > 0 {
		if c.Ingress.Stats() {
			err := c.ensureStatsService()
			if err != nil {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					core.EventTypeWarning,
					eventer.EventReasonIngressStatsServiceCreateFailed,
					"Failed to create HAProxy stats Service. Reason: %s",
					err.Error(),
				)
			} else {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					core.EventTypeNormal,
					eventer.EventReasonIngressStatsServiceCreateSuccessful,
					"Successfully created HAProxy stats Service %s",
					c.Ingress.StatsServiceName(),
				)
			}
		} else {
			err := c.ensureStatsServiceDeleted()
			if err != nil {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					core.EventTypeWarning,
					eventer.EventReasonIngressStatsServiceDeleteFailed,
					"Failed to delete HAProxy stats Service. Reason: %s",
					err.Error(),
				)
			} else {
				c.recorder.Eventf(
					c.Ingress.ObjectReference(),
					core.EventTypeNormal,
					eventer.EventReasonIngressStatsServiceDeleteSuccessful,
					"Successfully deleted HAProxy stats Service %s",
					c.Ingress.StatsServiceName(),
				)
			}
		}
	}
	return nil
}

func (c *nodePortController) EnsureFirewall(svc *core.Service) error {
	if c.CloudManager != nil {
		if fw, ok := c.CloudManager.Firewall(); ok {
			nodes, err := c.KubeClient.CoreV1().Nodes().List(metav1.ListOptions{})
			if err != nil {
				return err
			}
			hostnames := make([]string, len(nodes.Items))
			for i, node := range nodes.Items {
				hostnames[i] = node.Name
			}
			err = fw.EnsureFirewall(svc, hostnames)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *nodePortController) Delete() {
	err := c.deletePods()
	if err != nil {
		c.logger.Errorln(err)
	}
	err = c.deleteConfigMap()
	if err != nil {
		c.logger.Errorln(err)
	}
	if c.Opt.EnableRBAC {
		if err := c.ensureRBACDeleted(); err != nil {
			c.logger.Errorln(err)
		}
	}
	err = c.ensureServiceDeleted()
	if err != nil {
		c.logger.Errorln(err)
	}
	if c.CloudManager != nil {
		if fw, ok := c.CloudManager.Firewall(); ok {
			err = fw.EnsureFirewallDeleted(&core.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.Ingress.OffshootName(),
					Namespace: c.Ingress.Namespace,
				},
			})
			if err != nil {
				c.logger.Errorln(err)
			}
		}
	}
	monSpec, err := tools.Parse(c.Ingress.Annotations, api.EngressKey, api.DefaultExporterPortNumber)
	if err != nil {
		c.logger.Errorln(err)
	}
	if monSpec != nil {
		agent := agents.New(monSpec.Agent, c.KubeClient, c.CRDClient, c.PromClient)
		agent.Delete(c.Ingress.StatsAccessor())
	}
	if c.Ingress.Stats() {
		c.ensureStatsServiceDeleted()
	}
	return
}

func (c *nodePortController) waitForNodePortAssignment() error {
	return wait.Poll(time.Second*5, time.Minute*5, wait.ConditionFunc(func() (bool, error) {
		svc, err := c.KubeClient.CoreV1().
			Services(c.Ingress.Namespace).
			Get(c.Ingress.OffshootName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, port := range svc.Spec.Ports {
			if port.NodePort <= 0 {
				return false, errors.New("Port not assigned")
			}
		}
		return true, nil
	}))
}

func (c *nodePortController) newService() *core.Service {
	svc := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Ingress.OffshootName(),
			Namespace: c.Ingress.Namespace,
			Annotations: map[string]string{
				api.OriginAPISchema: c.Ingress.APISchema(),
				api.OriginName:      c.Ingress.GetName(),
			},
		},
		Spec: core.ServiceSpec{
			Type:        core.ServiceTypeNodePort,
			Ports:       []core.ServicePort{},
			Selector:    c.Ingress.OffshootLabels(),
			ExternalIPs: c.Ingress.Spec.ExternalIPs,
			// https://github.com/kubernetes/kubernetes/issues/33586
			// LoadBalancerSourceRanges: lbc.Config.Spec.LoadBalancerSourceRanges,
		},
	}
	svc.ObjectMeta = c.ensureOwnerReference(svc.ObjectMeta)

	// opening other tcp ports
	mappings, _ := c.Ingress.PortMappings(c.Opt.CloudProvider)
	for svcPort, target := range mappings {
		p := core.ServicePort{
			Name:       "tcp-" + strconv.Itoa(svcPort),
			Protocol:   "TCP",
			Port:       int32(svcPort),
			TargetPort: intstr.FromInt(target.PodPort),
			NodePort:   int32(target.NodePort),
		}
		svc.Spec.Ports = append(svc.Spec.Ports, p)
	}

	if ans, ok := c.Ingress.ServiceAnnotations(c.Opt.CloudProvider); ok {
		for k, v := range ans {
			svc.Annotations[k] = v
		}
	}
	if c.Ingress.KeepSourceIP() {
		// https://github.com/appscode/voyager/issues/276
		// ref: https://kubernetes.io/docs/tasks/services/source-ip/#source-ip-for-services-with-typeloadbalancer
		svc.Spec.ExternalTrafficPolicy = core.ServiceExternalTrafficPolicyTypeLocal
	}
	return svc
}

func (c *nodePortController) ensureService(old *api.Ingress) (*core.Service, error) {
	desired := c.newService()
	current, err := c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Get(desired.Name, metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		c.logger.Infof("Creating Service %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Create(desired)
	} else if err != nil {
		return nil, err
	}
	if svc, needsUpdate := c.serviceRequiresUpdate(current, desired, old); needsUpdate {
		c.logger.Infof("Updating Service %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.CoreV1().Services(c.Ingress.Namespace).Update(svc)
	}
	return current, nil
}

func (c *nodePortController) newPods() *apps.Deployment {
	deployment := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Ingress.OffshootName(),
			Namespace: c.Ingress.Namespace,
			Labels:    c.Ingress.OffshootLabels(),
			Annotations: map[string]string{
				api.OriginAPISchema: c.Ingress.APISchema(),
				api.OriginName:      c.Ingress.GetName(),
			},
		},

		Spec: apps.DeploymentSpec{
			Replicas: types.Int32P(c.Ingress.Replicas()),
			Selector: &metav1.LabelSelector{
				MatchLabels: c.Ingress.OffshootLabels(),
			},
			// pod templates.
			Template: core.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: c.Ingress.OffshootLabels(),
				},
				Spec: core.PodSpec{
					Affinity:         c.Ingress.Spec.Affinity,
					SchedulerName:    c.Ingress.Spec.SchedulerName,
					Tolerations:      c.Ingress.Spec.Tolerations,
					NodeSelector:     c.Ingress.NodeSelector(),
					ImagePullSecrets: c.Ingress.Spec.ImagePullSecrets,
					Containers: []core.Container{
						{
							Name:  "haproxy",
							Image: c.Opt.HAProxyImage,
							Args: []string{
								"--ingress-api-version=" + c.Ingress.APISchema(),
								"--ingress-name=" + c.Ingress.Name,
								"--cloud-provider=" + c.Opt.CloudProvider,
								"--v=3",
								fmt.Sprintf("--qps=%v", c.Opt.QPS),
								fmt.Sprintf("--burst=%v", c.Opt.Burst),
								"--boot-cmd=" + "/etc/sv/haproxy/reload",
								"--configmap=" + c.Ingress.OffshootName(),
								"--mount-location=" + "/etc/haproxy",
							},
							Env: []core.EnvVar{
								{
									Name:  analytics.Key,
									Value: c.Opt.AnalyticsClientID,
								},
							},
							Ports:     []core.ContainerPort{},
							Resources: c.Ingress.Spec.Resources,
							VolumeMounts: []core.VolumeMount{
								{
									Name:      TLSCertificateVolumeName,
									MountPath: "/etc/ssl/private/haproxy",
								},
							},
						},
					},
					Volumes: []core.Volume{
						{
							Name: TLSCertificateVolumeName,
							VolumeSource: core.VolumeSource{
								EmptyDir: &core.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	deployment.ObjectMeta = c.ensureOwnerReference(deployment.ObjectMeta)
	deployment.Spec.Template.Spec.Containers[0].Env = c.ensureEnvVars(deployment.Spec.Template.Spec.Containers[0].Env)

	if c.Opt.EnableRBAC {
		deployment.Spec.Template.Spec.ServiceAccountName = c.Ingress.OffshootName()
	}

	exporter, _ := c.getExporterSidecar()
	if exporter != nil {
		deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, *exporter)
	}

	// adding tcp ports to pod template
	for _, podPort := range c.Ingress.PodPorts() {
		p := core.ContainerPort{
			Name:          "tcp-" + strconv.Itoa(podPort),
			Protocol:      "TCP",
			ContainerPort: int32(podPort),
		}
		deployment.Spec.Template.Spec.Containers[0].Ports = append(deployment.Spec.Template.Spec.Containers[0].Ports, p)
	}

	if c.Ingress.Stats() {
		deployment.Spec.Template.Spec.Containers[0].Ports = append(deployment.Spec.Template.Spec.Containers[0].Ports, core.ContainerPort{
			Name:          api.StatsPortName,
			Protocol:      "TCP",
			ContainerPort: int32(c.Ingress.StatsPort()),
		})
	}

	if ans, ok := c.Ingress.PodsAnnotations(); ok {
		deployment.Spec.Template.Annotations = ans
	}

	if len(c.Ingress.ErrorFilesConfigMapName()) > 0 {
		deployment.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			deployment.Spec.Template.Spec.Containers[0].VolumeMounts,
			core.VolumeMount{
				Name:      ErrorFilesVolumeName,
				MountPath: ErrorFilesLocation,
			})

		deployment.Spec.Template.Spec.Volumes = append(
			deployment.Spec.Template.Spec.Volumes,
			core.Volume{
				Name: ErrorFilesVolumeName,
				VolumeSource: core.VolumeSource{
					ConfigMap: &core.ConfigMapVolumeSource{
						LocalObjectReference: core.LocalObjectReference{
							Name: c.Ingress.ErrorFilesConfigMapName(),
						},
					},
				},
			})
	}
	return deployment
}

func (c *nodePortController) ensurePods(old *api.Ingress) (*apps.Deployment, error) {
	desired := c.newPods()
	current, err := c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Get(desired.Name, metav1.GetOptions{})
	if kerr.IsNotFound(err) {
		c.logger.Infof("Creating Deployment %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Create(desired)
	} else if err != nil {
		return nil, err
	}

	needsUpdate := false

	// annotations
	if current.Annotations == nil {
		current.Annotations = make(map[string]string)
	}
	oldAnn := map[string]string{}
	if old != nil {
		if a, ok := old.PodsAnnotations(); ok {
			oldAnn = a
		}
	}
	for k, v := range desired.Annotations {
		if cv, found := current.Annotations[k]; !found || cv != v {
			current.Annotations[k] = v
			needsUpdate = true
		}
		delete(oldAnn, k)
	}
	for k := range oldAnn {
		if _, ok := current.Annotations[k]; ok {
			delete(current.Annotations, k)
			needsUpdate = true
		}
	}

	if !reflect.DeepEqual(current.Spec.Selector, desired.Spec.Selector) {
		needsUpdate = true
		current.Spec.Selector = desired.Spec.Selector
	}
	if !reflect.DeepEqual(current.Spec.Template.ObjectMeta, desired.Spec.Template.ObjectMeta) {
		needsUpdate = true
		current.Spec.Template.ObjectMeta = desired.Spec.Template.ObjectMeta
	}
	if !reflect.DeepEqual(current.Spec.Template.Annotations, desired.Spec.Template.Annotations) {
		needsUpdate = true
		current.Spec.Template.Annotations = desired.Spec.Template.Annotations
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Affinity, desired.Spec.Template.Spec.Affinity) {
		needsUpdate = true
		current.Spec.Template.Spec.Affinity = desired.Spec.Template.Spec.Affinity
	}
	if current.Spec.Template.Spec.SchedulerName != desired.Spec.Template.Spec.SchedulerName {
		needsUpdate = true
		current.Spec.Template.Spec.SchedulerName = desired.Spec.Template.Spec.SchedulerName
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Tolerations, desired.Spec.Template.Spec.Tolerations) {
		needsUpdate = true
		current.Spec.Template.Spec.Tolerations = desired.Spec.Template.Spec.Tolerations
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.NodeSelector, desired.Spec.Template.Spec.NodeSelector) {
		needsUpdate = true
		current.Spec.Template.Spec.NodeSelector = desired.Spec.Template.Spec.NodeSelector
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.ImagePullSecrets, desired.Spec.Template.Spec.ImagePullSecrets) {
		needsUpdate = true
		current.Spec.Template.Spec.ImagePullSecrets = desired.Spec.Template.Spec.ImagePullSecrets
	}
	if !meta.Equal(current.Spec.Template.Spec.Containers, desired.Spec.Template.Spec.Containers) {
		needsUpdate = true
		current.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	}
	if !reflect.DeepEqual(current.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		needsUpdate = true
		current.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
	}
	if current.Spec.Template.Spec.ServiceAccountName != desired.Spec.Template.Spec.ServiceAccountName {
		needsUpdate = true
		current.Spec.Template.Spec.ServiceAccountName = desired.Spec.Template.Spec.ServiceAccountName
	}
	if needsUpdate {
		c.logger.Infof("Updating Deployment %s/%s", desired.Namespace, desired.Name)
		return c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Update(current)
	}
	return current, nil
}

func (c *nodePortController) deletePods() error {
	c.logger.Infof("Deleting Deployment %s/%s", c.Ingress.Namespace, c.Ingress.OffshootName())
	policy := metav1.DeletePropagationForeground
	err := c.KubeClient.AppsV1beta1().Deployments(c.Ingress.Namespace).Delete(c.Ingress.OffshootName(), &metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil {
		return err
	}
	return c.deletePodsForSelector(&metav1.LabelSelector{MatchLabels: c.Ingress.OffshootLabels()})
}
