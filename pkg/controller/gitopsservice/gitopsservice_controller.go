package gitopsservice

import (
	"context"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"

	argoapp "github.com/argoproj-labs/argocd-operator/pkg/apis/argoproj/v1alpha1"
	monitoringv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/go-logr/logr"

	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/pkg/apis/pipelines/v1alpha1"
	argocd "github.com/redhat-developer/gitops-operator/pkg/controller/argocd"
	"github.com/redhat-developer/gitops-operator/pkg/controller/util"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_gitopsservice")

// defaults must some somewhere else..
var (
	port                       int32  = 8080
	portTLS                    int32  = 8443
	backendImage               string = "quay.io/redhat-developer/gitops-backend:v0.0.1"
	backendImageEnvName               = "BACKEND_IMAGE"
	serviceName                       = "cluster"
	insecureEnvVar                    = "INSECURE"
	insecureEnvVarValue               = "true"
	serviceNamespace                  = "openshift-gitops"
	depracatedServiceNamespace        = "openshift-pipelines-app-delivery"
	clusterVersionName                = "version"
)

const (
	readRoleNameFormat        = "%s-read"
	readRoleBindingNameFormat = "%s-prometheus-k8s-read-binding"
	alertRuleName = "gitops-operator-argocd-alerts"
)

// Add creates a new GitopsService Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileGitopsService{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {

	reqLogger := log.WithValues()
	reqLogger.Info("Watching GitopsService")

	pred := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Ignore updates to CR status in which case metadata.Generation does not change
			return e.MetaOld.GetGeneration() != e.MetaNew.GetGeneration()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Evaluates to false if the object has been confirmed deleted.
			return !e.DeleteStateUnknown
		},
	}

	// Create a new controller
	c, err := controller.New("gitopsservice-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource GitopsService
	err = c.Watch(&source.Kind{Type: &pipelinesv1alpha1.GitopsService{}}, &handler.EnqueueRequestForObject{}, pred)
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &routev1.Route{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &pipelinesv1alpha1.GitopsService{},
	}, pred)

	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &corev1.Service{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &pipelinesv1alpha1.GitopsService{},
	}, pred)
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &pipelinesv1alpha1.GitopsService{},
	}, pred)
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &corev1.ConfigMap{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &pipelinesv1alpha1.GitopsService{},
	})
	if err != nil {
		return err
	}

	client := mgr.GetClient()

	gitopsServiceRef := newGitopsService()
	err = client.Create(context.TODO(), gitopsServiceRef)
	if err != nil {
		reqLogger.Error(err, "Failed to create GitOps service instance")
	}
	return nil
}

// blank assignment to verify that ReconcileGitopsService implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileGitopsService{}

// ReconcileGitopsService reconciles a GitopsService object
type ReconcileGitopsService struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a GitopsService object and makes changes based on the state read
// and what is in the GitopsService.Spec
func (r *ReconcileGitopsService) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling GitopsService")

	// Fetch the GitopsService instance
	instance := &pipelinesv1alpha1.GitopsService{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: serviceName}, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	namespace, err := GetBackendNamespace(r.client)
	if err != nil {
		return reconcile.Result{}, err
	}

	namespaceRef := newNamespace(namespace)
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: namespace}, &corev1.Namespace{})
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Namespace", "Name", namespace)
		err = r.client.Create(context.TODO(), namespaceRef)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	serviceNamespacedName := types.NamespacedName{
		Name:      serviceName,
		Namespace: namespace,
	}

	argoCDIdentifier := fmt.Sprintf("argocd-%s", request.Name)
	defaultArgoCDInstance, err := argocd.NewCR(argoCDIdentifier, serviceNamespace)

	// The operator decides the namespace based on the version of the cluster it is installed in
	// 4.6 Cluster: Backend in openshift-pipelines-app-delivery namespace and argocd in openshift-gitops namespace
	// 4.7 Cluster: Both backend and argocd instance in openshift-gitops namespace
	argocdNS := newNamespace(defaultArgoCDInstance.Namespace)
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: argocdNS.Name}, &corev1.Namespace{})
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Namespace", "Name", argocdNS.Name)
		err = r.client.Create(context.TODO(), argocdNS)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Set GitopsService instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, defaultArgoCDInstance, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	existingArgoCD := &argoapp.ArgoCD{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: defaultArgoCDInstance.Name, Namespace: defaultArgoCDInstance.Namespace}, existingArgoCD)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new ArgoCD instance", "Namespace", defaultArgoCDInstance.Namespace, "Name", defaultArgoCDInstance.Name)
		err = r.client.Create(context.TODO(), defaultArgoCDInstance)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// Create role to grant read permission to the openshift metrics stack
	readRoleRef := newReadRole(defaultArgoCDInstance.Namespace)
	reqLogger.Info("Creating new read role",
		"Namespace", readRoleRef.Namespace, "Name", readRoleRef.Name)
	err = r.client.Create(context.TODO(), readRoleRef)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			reqLogger.Info("Read role already exists",
				"Namespace", readRoleRef.Namespace, "Name", readRoleRef.Name)
		} else {
			reqLogger.Error(err, "Failed to create read role",
				"Namespace", readRoleRef.Namespace, "Name", readRoleRef.Name)
			return reconcile.Result{}, err
		}
	}

	// Create role binding to grant read permission to the openshift metrics stack
	readRoleBindingRef := newReadRoleBinding(defaultArgoCDInstance.Namespace)
	reqLogger.Info("Creating new read role binding",
		"Namespace", readRoleBindingRef.Namespace, "Name", readRoleBindingRef.Name, )
	err = r.client.Create(context.TODO(), readRoleBindingRef)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			reqLogger.Info("Read role bindingalready exists",
				"Namespace", readRoleBindingRef.Namespace, "Name", readRoleBindingRef.Name)
		} else {
			reqLogger.Error(err, "Failed to create read role binding",
				"Namespace", readRoleBindingRef.Namespace, "Name", readRoleBindingRef.Name, )
			return reconcile.Result{}, err
		}
	}

	// ServiceMonitor for ArgoCD application metrics
	serviceMonitorLabel := fmt.Sprintf("%s-metrics", argoCDIdentifier)
	serviceMonitorName := argoCDIdentifier
	err = r.createServiceMonitorIfAbsent(defaultArgoCDInstance.Namespace, serviceMonitorName, serviceMonitorLabel, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// ServiceMonitor for ArgoCD API server metrics
	serviceMonitorLabel = fmt.Sprintf("%s-server-metrics", argoCDIdentifier)
	serviceMonitorName = fmt.Sprintf("%s-server", argoCDIdentifier)
	err = r.createServiceMonitorIfAbsent(defaultArgoCDInstance.Namespace, serviceMonitorName, serviceMonitorLabel, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// ServiceMonitor for ArgoCD repo server metrics
	serviceMonitorLabel = fmt.Sprintf("%s-repo-server", argoCDIdentifier)
	serviceMonitorName = fmt.Sprintf("%s-repo-server", argoCDIdentifier)
	err = r.createServiceMonitorIfAbsent(defaultArgoCDInstance.Namespace, serviceMonitorName, serviceMonitorLabel, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Create alert rules
	err = r.createPrometheusRuleIfAbsent(defaultArgoCDInstance.Namespace, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Define a new Pod object
	deploymentObj := newBackendDeployment(serviceNamespacedName)

	// Set GitopsService instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, deploymentObj, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	// Check if this Deployment already exists
	found := &appsv1.Deployment{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: deploymentObj.Name, Namespace: deploymentObj.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Deployment", "Namespace", deploymentObj.Namespace, "Name", deploymentObj.Name)
		err = r.client.Create(context.TODO(), deploymentObj)
		if err != nil {
			return reconcile.Result{}, err
		}
	}
	serviceRef := newBackendService(serviceNamespacedName)
	// Set GitopsService instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, serviceRef, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	// Check if this Service already exists
	existingServiceRef := &corev1.Service{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: serviceRef.Name, Namespace: serviceRef.Namespace}, existingServiceRef)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Service", "Namespace", deploymentObj.Namespace, "Name", deploymentObj.Name)
		err = r.client.Create(context.TODO(), serviceRef)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	routeRef := newBackendRoute(serviceNamespacedName)
	// Set GitopsService instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, routeRef, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	existingRoute := &routev1.Route{}

	err = r.client.Get(context.TODO(), types.NamespacedName{Name: routeRef.Name, Namespace: routeRef.Namespace}, existingRoute)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new Route", "Namespace", routeRef.Namespace, "Name", routeRef.Name)
		err = r.client.Create(context.TODO(), routeRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	return r.reconcileCLIServer(instance, request)
}

// GetBackendNamespace returns the backend service namespace based on OpenShift Cluster version
func GetBackendNamespace(client client.Client) (string, error) {
	version, err := util.GetClusterVersion(client)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(version, "4.6") {
		return depracatedServiceNamespace, nil
	}
	return serviceNamespace, nil
}

func (r *ReconcileGitopsService) createServiceMonitorIfAbsent(namespace, name, serviceMonitorLabel string, reqLogger logr.Logger) (error) {
	existingServiceMonitor := &monitoringv1.ServiceMonitor{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, existingServiceMonitor)
	if err == nil {
		reqLogger.Info("A ServiceMonitor instance already exists",
			"Namespace", existingServiceMonitor.Namespace, "Name", existingServiceMonitor.Name)
		return nil
	}
	if errors.IsNotFound(err) {
		serviceMonitor := newServiceMonitor(namespace, name, serviceMonitorLabel)
		reqLogger.Info("Creating a new ServiceMonitor instance",
			"Namespace", serviceMonitor.Namespace, "Name", serviceMonitor.Name)
		err = r.client.Create(context.TODO(), serviceMonitor)
		if err != nil {
			reqLogger.Info("Error creating a new ServiceMonitor instance",
				"Namespace", serviceMonitor.Namespace, "Name", serviceMonitor.Name)
			return err
		}
		return nil
	}
	reqLogger.Info("Error querying for ServiceMonitor", name, "Namespace", namespace, "Name")
	return err
}

func (r *ReconcileGitopsService) createPrometheusRuleIfAbsent(namespace string, reqLogger logr.Logger) error {
	existingAlertRule := &monitoringv1.PrometheusRule{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: alertRuleName, Namespace: namespace}, existingAlertRule)
	if err == nil {
		reqLogger.Info("An alert rule instance already exists",
			"Namespace", existingAlertRule.Namespace, "Name", existingAlertRule.Name)
		return nil
	}
	if errors.IsNotFound(err) {
		alertRule := newPrometheusRule(namespace)
		reqLogger.Info("Creating new alert rule",
			"Namespace", alertRule.Namespace, "Name", alertRule.Name)
		err := r.client.Create(context.TODO(), alertRule)
		if err != nil {
			reqLogger.Error(err, "Error creating a new alert rule",
				"Namespace", alertRule.Namespace, "Name", alertRule.Name)
			return err
		}
		return nil
	}
	reqLogger.Info("Error querying for existing alert rule",
		"Namespace", namespace, "Name", alertRuleName)
	return err
}

func objectMeta(resourceName string, namespace string, opts ...func(*metav1.ObjectMeta)) metav1.ObjectMeta {
	objectMeta := metav1.ObjectMeta{
		Name:      resourceName,
		Namespace: namespace,
	}
	for _, o := range opts {
		o(&objectMeta)
	}
	return objectMeta
}

func newBackendDeployment(ns types.NamespacedName) *appsv1.Deployment {
	image := os.Getenv(backendImageEnvName)
	if image == "" {
		image = backendImage
	}
	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  ns.Name,
				Image: image,
				Ports: []corev1.ContainerPort{
					{
						Name:          "http",
						Protocol:      corev1.ProtocolTCP,
						ContainerPort: port, // should come from flag
					},
				},
				Env: []corev1.EnvVar{
					{
						Name:  insecureEnvVar,
						Value: insecureEnvVarValue,
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						MountPath: "/etc/gitops/ssl",
						Name:      "backend-ssl",
						ReadOnly:  true,
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: "backend-ssl",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: ns.Name,
					},
				},
			},
		},
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/name": ns.Name,
			},
		},
		Spec: podSpec,
	}

	var replicas int32 = 1
	deploymentSpec := appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": ns.Name,
			},
		},
		Template: template,
	}

	deploymentObj := &appsv1.Deployment{
		ObjectMeta: objectMeta(ns.Name, ns.Namespace),
		Spec:       deploymentSpec,
	}

	return deploymentObj
}

func newBackendService(ns types.NamespacedName) *corev1.Service {

	spec := corev1.ServiceSpec{
		Ports: []corev1.ServicePort{
			{
				Port:       port,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(int(port)),
			},
		},
		Selector: map[string]string{
			"app.kubernetes.io/name": ns.Name,
		},
	}
	svc := &corev1.Service{
		ObjectMeta: objectMeta(ns.Name, ns.Namespace, func(o *metav1.ObjectMeta) {
			o.Annotations = map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": ns.Name,
			}
		}),
		Spec: spec,
	}
	return svc
}

func newBackendRoute(ns types.NamespacedName) *routev1.Route {
	routeSpec := routev1.RouteSpec{
		To: routev1.RouteTargetReference{
			Kind: "Service",
			Name: ns.Name,
		},
		Port: &routev1.RoutePort{
			TargetPort: intstr.IntOrString{IntVal: port},
		},
		TLS: &routev1.TLSConfig{
			Termination:                   routev1.TLSTerminationReencrypt,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyAllow,
		},
	}

	routeObj := &routev1.Route{
		ObjectMeta: objectMeta(ns.Name, ns.Namespace),
		Spec:       routeSpec,
	}

	return routeObj
}

func newNamespace(ns string) *corev1.Namespace {
	objectMeta := metav1.ObjectMeta{
		Name: ns,
		Labels: map[string]string{
			// Enable full-fledged support for integration with cluster monitoring.
			"openshift.io/cluster-monitoring": "true",
		},
	}
	return &corev1.Namespace{
		ObjectMeta: objectMeta,
	}
}

func newGitopsService() *pipelinesv1alpha1.GitopsService {
	return &pipelinesv1alpha1.GitopsService{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceName,
		},
		Spec: pipelinesv1alpha1.GitopsServiceSpec{},
	}
}

func newReadRole(namespace string) *rbacv1.Role {
	objectMeta := metav1.ObjectMeta{
		Name: fmt.Sprintf(readRoleNameFormat, namespace),
		Namespace: namespace,
	}
	rules := []rbacv1.PolicyRule {
		{
			APIGroups: []string{""},
			Resources: []string{"endpoints", "services", "pods"},
			Verbs:     []string{"get", "list", "watch"},
		},
	}
	return &rbacv1.Role{
		ObjectMeta: objectMeta,
		Rules: rules,
	}
}

func newReadRoleBinding(namespace string) *rbacv1.RoleBinding {
	objectMeta := metav1.ObjectMeta{
		Name: fmt.Sprintf(readRoleBindingNameFormat, namespace),
		Namespace: namespace,
	}
	roleRef := rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind: "Role",
		Name: fmt.Sprintf(readRoleNameFormat, namespace),
	}
	subjects := []rbacv1.Subject{
		{
			Kind: "ServiceAccount",
			Name: "prometheus-k8s",
			Namespace: "openshift-monitoring",
		},
	}
	return &rbacv1.RoleBinding{
		ObjectMeta: objectMeta,
		RoleRef: roleRef,
		Subjects: subjects,
	}
}

func newServiceMonitor(namespace, name, matchLabel string) *monitoringv1.ServiceMonitor {
	objectMeta := metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"release": "prometheus-operator",
		},
	}
	spec := monitoringv1.ServiceMonitorSpec{
		Selector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": matchLabel,
			},
		},
		Endpoints: []monitoringv1.Endpoint{
			{
				Port: "metrics",
			},
		},
	}
	return &monitoringv1.ServiceMonitor{
		ObjectMeta: objectMeta,
		Spec: spec,
	}
}

func newPrometheusRule(namespace string) *monitoringv1.PrometheusRule {
	objectMeta := metav1.ObjectMeta{
		Name:      alertRuleName,
		Namespace: namespace,
	}
	spec := monitoringv1.PrometheusRuleSpec{
		Groups: []monitoringv1.RuleGroup{
			{
				Name: "GitOpsOperatorArgoCD",
				Rules: []monitoringv1.Rule{
					{
						Alert: "ArgoCDSyncAlert",
						Annotations: map[string]string{
							"message": "ArgoCD application {{ $labels.name }} is out of sync",
						},
						Expr: intstr.IntOrString{
							Type: intstr.String,
							StrVal: "argocd_app_info{sync_status=\"OutOfSync\"} > 0",
						},
						Labels: map[string]string{
							"severity": "warning",
						},
					},
				},
			},
		},
	}
	return &monitoringv1.PrometheusRule{
		ObjectMeta: objectMeta,
		Spec: spec,
	}
}