package gitopsservice

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"os"

	appsv1 "k8s.io/api/apps/v1"

	argoapp "github.com/argoproj-labs/argocd-operator/pkg/apis/argoproj/v1alpha1"
	monitoring "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	routev1 "github.com/openshift/api/route/v1"

	pipelinesv1alpha1 "github.com/redhat-developer/gitops-operator/pkg/apis/pipelines/v1alpha1"
	argocd "github.com/redhat-developer/gitops-operator/pkg/controller/argocd"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	rbacv1 "k8s.io/api/rbac/v1"
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
	port                int32  = 8080
	backendImage        string = "quay.io/redhat-developer/gitops-backend:v0.0.1"
	backendImageEnvName        = "BACKEND_IMAGE"
	serviceName                = "cluster"
	serviceNamespace           = "openshift-gitops"
	insecureEnvVar             = "INSECURE"
	insecureEnvVarValue        = "true"
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

	namespaceRef := newNamespace()
	err = client.Create(context.TODO(), namespaceRef)
	if err != nil {
		reqLogger.Error(err, "Failed to create namespace", "Namespace", serviceNamespace)
	}

	readRoleRef := newReadRole()
	reqLogger.Info("Creating new read role", "Name", readRoleRef.Name)
	err = client.Create(context.TODO(), readRoleRef)
	if err != nil {
		reqLogger.Error(err, "Failed to create read role", "Name", readRoleRef.Name)
	}

	readRoleBindingRef := newReadRoleBinding()
	reqLogger.Info("Creating new read role binding", "Name", readRoleBindingRef.Name)
	err = client.Create(context.TODO(), readRoleBindingRef)
	if err != nil {
		reqLogger.Error(err, "Failed to create read role binding", "Name", readRoleBindingRef.Name)
	}

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
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
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

	argoCDIdentifier := fmt.Sprintf("argocd-%s", request.Name)
	defaultArgoCDInstance, err := argocd.NewCR(argoCDIdentifier, serviceNamespace)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Set GitopsService instance as the owner and controller
	if err := controllerutil.SetControllerReference(instance, defaultArgoCDInstance, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	existingArgoCD := &argoapp.ArgoCD{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: defaultArgoCDInstance.Name, Namespace: serviceNamespace}, existingArgoCD)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a new ArgoCD instance", "Namespace", defaultArgoCDInstance.Namespace, "Name", defaultArgoCDInstance.Name)
		err = r.client.Create(context.TODO(), defaultArgoCDInstance)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// ServiceMonitor for ArgoCD application metrics
	serviceMonitorLabel := fmt.Sprintf("%s-metrics", argoCDIdentifier)
	err = r.createServiceMonitorIfAbsent(serviceMonitorLabel, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// ServiceMonitor for ArgoCD API server metrics
	serviceMonitorLabel = fmt.Sprintf("%s-server-metrics", argoCDIdentifier)
	err = r.createServiceMonitorIfAbsent(serviceMonitorLabel, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// ServiceMonitor for ArgoCD repo server metrics
	serviceMonitorLabel = fmt.Sprintf("%s-repo-server", argoCDIdentifier)
	err = r.createServiceMonitorIfAbsent(serviceMonitorLabel, reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	err = r.createPrometheusRuleIfAbsent(reqLogger)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Define a new Pod object
	deploymentObj := newBackendDeployment(request.Name, serviceNamespace)

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
	serviceRef := newBackendService(request.Name, serviceNamespace)
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

	routeRef := newBackendRoute(request.Name, serviceNamespace)
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

	return reconcile.Result{}, nil
}

func (r *ReconcileGitopsService) createServiceMonitorIfAbsent(serviceMonitorLabel string, reqLogger logr.Logger) (error) {
	existingServiceMonitor := &monitoring.ServiceMonitor{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: serviceMonitorLabel, Namespace: serviceNamespace}, existingServiceMonitor)
	if err == nil {
		reqLogger.Info("A ServiceMonitor instance already exists",
			"Name", existingServiceMonitor.Name)
		return nil
	}
	if errors.IsNotFound(err) {
		serviceMonitor := newServiceMonitor(serviceMonitorLabel)
		reqLogger.Info("Creating a new ServiceMonitor instance", "Name", serviceMonitor.Name)
		err = r.client.Create(context.TODO(), serviceMonitor)
		if err != nil {
			reqLogger.Info("Error creating a new ServiceMonitor instance","Name", serviceMonitor.Name)
			return err
		}
		return nil
	}
	reqLogger.Info("Error querying for ServiceMonitor", "Name", serviceMonitorLabel)
	return err
}

func (r *ReconcileGitopsService) createPrometheusRuleIfAbsent(reqLogger logr.Logger) error {
	existingAlertRule := &monitoring.PrometheusRule{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: alertRuleName, Namespace: serviceNamespace}, existingAlertRule)
	if err == nil {
		reqLogger.Info("An alert rule instance already exists", "Name", existingAlertRule.Name)
		return nil
	}
	if errors.IsNotFound(err) {
		alertRule := newPrometheusRule()
		reqLogger.Info("Creating new alert rule", "Name", alertRule.Name)
		err := r.client.Create(context.TODO(), alertRule)
		if err != nil {
			reqLogger.Error(err, "Error creating a new alert rule", "Name", alertRule.Name)
			return err
		}
		return nil
	}
	reqLogger.Info("Error querying for existing alert rule", "Name", alertRuleName)
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

func newBackendDeployment(name, namespace string) *appsv1.Deployment {
	image := os.Getenv(backendImageEnvName)
	if image == "" {
		image = backendImage
	}
	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  name,
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
						SecretName: serviceNamespace,
					},
				},
			},
		},
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/name": serviceName,
			},
		},
		Spec: podSpec,
	}

	var replicas int32 = 1
	deploymentSpec := appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": name,
			},
		},
		Template: template,
	}

	deploymentObj := &appsv1.Deployment{
		ObjectMeta: objectMeta(name, namespace),
		Spec:       deploymentSpec,
	}

	return deploymentObj
}

func newBackendService(name, namespace string) *corev1.Service {

	spec := corev1.ServiceSpec{
		Ports: []corev1.ServicePort{
			{
				Port:       port,
				Protocol:   corev1.ProtocolTCP,
				TargetPort: intstr.FromInt(int(port)),
			},
		},
		Selector: map[string]string{
			"app.kubernetes.io/name": name,
		},
	}
	svc := &corev1.Service{
		ObjectMeta: objectMeta(serviceName, serviceNamespace, func(o *metav1.ObjectMeta) {
			o.Annotations = map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": namespace,
			}
		}),
		Spec: spec,
	}
	return svc
}

func newBackendRoute(name, namespace string) *routev1.Route {
	routeSpec := routev1.RouteSpec{
		To: routev1.RouteTargetReference{
			Kind: "Service",
			Name: name,
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
		ObjectMeta: objectMeta(name, namespace),
		Spec:       routeSpec,
	}

	return routeObj
}

func newNamespace() *corev1.Namespace {
	objectMeta := metav1.ObjectMeta{
		Name: serviceNamespace,
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

func newReadRole() *rbacv1.Role {
	objectMeta := metav1.ObjectMeta{
		Name: fmt.Sprintf(readRoleNameFormat, serviceNamespace),
		Namespace: serviceNamespace,
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

func newReadRoleBinding() *rbacv1.RoleBinding {
	objectMeta := metav1.ObjectMeta{
		Name: fmt.Sprintf(readRoleBindingNameFormat, serviceNamespace),
		Namespace: serviceNamespace,
	}
	roleRef := rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind: "Role",
		Name: fmt.Sprintf(readRoleNameFormat, serviceNamespace),
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

func newServiceMonitor(matchLabel string) *monitoring.ServiceMonitor {
	// we also use the matchLabel for the name of the ServiceMonitor
	objectMeta := metav1.ObjectMeta{
		Name:      matchLabel,
		Namespace: serviceNamespace,
		Labels: map[string]string{
			"release": "prometheus-operator",
		},
	}
	spec := monitoring.ServiceMonitorSpec{
		Selector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app.kubernetes.io/name": matchLabel,
			},
		},
		Endpoints: []monitoring.Endpoint{
			{
				Port: "metrics",
			},
		},
	}
	return &monitoring.ServiceMonitor{
		ObjectMeta: objectMeta,
		Spec: spec,
	}
}

func newPrometheusRule() *monitoring.PrometheusRule {
	objectMeta := metav1.ObjectMeta{
		Name:      alertRuleName,
		Namespace: serviceNamespace,
	}
	spec := monitoring.PrometheusRuleSpec{
		Groups: []monitoring.RuleGroup{
			{
				Name: "GitOpsOperatorArgoCD",
				Rules: []monitoring.Rule{
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
	return &monitoring.PrometheusRule{
		ObjectMeta: objectMeta,
		Spec: spec,
	}
}