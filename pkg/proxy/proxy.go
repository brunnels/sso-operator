package proxy

import (
	"crypto/rand"
	"fmt"
	"path/filepath"

	"github.com/coreos/dex/api"
	apiv1 "github.com/jenkins-x/sso-operator/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/sso-operator/pkg/kubernetes"
	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	configPath       = "/config/oauth2_proxy.cfg"
	configVolumeName = "proxy-config"
	configSecretName = "proxy-secret" // #nosec
	portName         = "proxy-port"
	port             = 4180
	healthPath       = "/ping"
	replicas         = 1
	publicPort       = 80
	cookieSecretLen  = 32
	fakeURL          = "http://fake-oauth2-proxy"
)

// FakeRedirectURL builds a fake redirect URL for oauth2 proxy
func FakeRedirectURL() string {
	return RedirectURL(fakeURL)
}

// RedirectURL build the redirect URL for oauth2 proxy
func RedirectURL(URL string) string {
	return fmt.Sprintf("%s/oauth2/callback", URL)
}

// Deploy deploys the oauth2 proxy
func Deploy(sso *apiv1.SSO, oidcClient *api.Client) error {
	labels := map[string]string{"app": sso.Spec.UpstreamService}

	secret, err := proxySecret(sso, oidcClient, fakeURL, labels)
	if err != nil {
		return errors.Wrap(err, "creating oauth2_proxy config")
	}
	secret.SetOwnerReferences(append(secret.GetOwnerReferences(), ownerRef(sso)))
	err = sdk.Create(secret)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "creating oauth2_proxy secret")
	}

	podTempl := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sso.GetName(),
			Namespace: sso.GetNamespace(),
			Labels:    labels,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{proxyContainer(sso)},
			Volumes: []v1.Volume{{
				Name: configVolumeName,
				VolumeSource: v1.VolumeSource{
					Secret: &v1.SecretVolumeSource{
						SecretName: configSecretName,
					},
				},
			}},
		},
	}

	var replicas int32 = replicas
	d := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sso.GetName(),
			Namespace: sso.GetNamespace(),
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: podTempl,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: func(a intstr.IntOrString) *intstr.IntOrString { return &a }(intstr.FromInt(1)),
					MaxSurge:       func(a intstr.IntOrString) *intstr.IntOrString { return &a }(intstr.FromInt(1)),
				},
			},
		},
	}

	d.SetOwnerReferences(append(d.GetOwnerReferences(), ownerRef(sso)))

	err = sdk.Create(d)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "creating oauth2_proxy deployment")
	}

	annotations := map[string]string{
		"fabric8.io/expose":              "true",
		"fabric8.io/ingress.annotations": "kubernetes.io/ingress.class: nginx",
	}
	svc := &v1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        sso.GetName(),
			Namespace:   sso.GetNamespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				Name:       portName,
				Protocol:   v1.ProtocolTCP,
				Port:       publicPort,
				TargetPort: intstr.FromInt(port),
			}},
			Selector: labels,
		},
	}

	svc.SetOwnerReferences(append(svc.GetOwnerReferences(), ownerRef(sso)))

	err = sdk.Create(svc)
	if err != nil && apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "creating oauth2_proxy service")
	}
	return nil
}

func ownerRef(sso *apiv1.SSO) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: apiv1.SchemeGroupVersion.String(),
		Kind:       apiv1.SSOKind,
		Name:       sso.Name,
		UID:        sso.UID,
		Controller: &controller,
	}
}

func proxyContainer(sso *apiv1.SSO) v1.Container {
	return v1.Container{
		Name:            sso.GetName(),
		Image:           fmt.Sprintf("%s:%s", sso.Spec.ProxyImage, sso.Spec.ProxyImageTag),
		ImagePullPolicy: v1.PullIfNotPresent,
		Args:            []string{fmt.Sprintf("--config=%s", configPath)},
		Ports: []v1.ContainerPort{{
			Name:          portName,
			ContainerPort: int32(port),
			Protocol:      v1.ProtocolTCP,
		}},
		Resources: sso.Spec.ProxyResources,
		VolumeMounts: []v1.VolumeMount{{
			Name:      configVolumeName,
			ReadOnly:  true,
			MountPath: filepath.Dir(configPath),
		}},
		LivenessProbe: &v1.Probe{
			Handler: v1.Handler{
				HTTPGet: &v1.HTTPGetAction{
					Path:   healthPath,
					Port:   intstr.FromInt(port),
					Scheme: v1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      10,
			PeriodSeconds:       60,
			FailureThreshold:    3,
		},
		ReadinessProbe: &v1.Probe{
			Handler: v1.Handler{
				HTTPGet: &v1.HTTPGetAction{
					Path:   healthPath,
					Port:   intstr.FromInt(port),
					Scheme: v1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 10,
			TimeoutSeconds:      10,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
	}
}

func proxySecret(sso *apiv1.SSO, client *api.Client, proxyURL string,
	labels map[string]string) (*v1.Secret, error) {
	cookieSecret, err := generateSecret(cookieSecretLen)
	if err != nil {
		return nil, errors.Wrap(err, "generating cookie secret")
	}
	upstreamURL, err := getUpstreamURL(sso.Spec.UpstreamService, sso.Namespace)
	if err != nil {
		return nil, errors.Wrap(err, "getting the upstream service URL")
	}

	proxyConfig := &Config{
		Port:          port,
		ClientID:      client.GetId(),
		ClientSecret:  client.GetSecret(),
		OIDCIssuerURL: sso.Spec.OIDCIssuerURL,
		RedirectURL:   FakeRedirectURL(),
		LoginURL:      fmt.Sprintf("%s/auth", sso.Spec.OIDCIssuerURL),
		RedeemURL:     fmt.Sprintf("%s/token", sso.Spec.OIDCIssuerURL),
		Upstream:      upstreamURL,
		Cookie: Cookie{
			Name:     sso.Spec.CookieSpec.Name,
			Secret:   cookieSecret,
			Domain:   proxyURL,
			Expire:   sso.Spec.CookieSpec.Expire,
			Refresh:  sso.Spec.CookieSpec.Refresh,
			Secure:   sso.Spec.CookieSpec.Secure,
			HTTPOnly: sso.Spec.CookieSpec.HTTPOnly,
		},
	}

	config, err := renderConfig(proxyConfig)
	if err != nil {
		return nil, errors.Wrap(err, "rendering oauth2_proxy config")
	}

	secret := &v1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      configSecretName,
			Namespace: sso.Namespace,
			Labels:    labels,
		},
		StringData: map[string]string{
			filepath.Base(configPath): config,
		},
		Type: v1.SecretTypeOpaque,
	}
	return secret, nil
}

func generateSecret(len int) (string, error) {
	b := make([]byte, len)
	_, err := rand.Read(b)
	if err != nil {
		return "", errors.Wrap(err, "generating random")
	}
	return string(b), nil
}

func getUpstreamURL(upstreamService string, namespace string) (string, error) {
	kubeClient, err := kubernetes.GetClientset()
	if err != nil {
		return "", errors.Wrap(err, "creating k8s client")
	}

	serviceList, err := kubeClient.CoreV1().Services(namespace).List(metav1.ListOptions{})
	if err != nil {
		return "", errors.Wrapf(err, "listing services in namespace '%s'", namespace)
	}
	var foundService *v1.Service
	for _, service := range serviceList.Items {
		if service.Name == upstreamService {
			foundService = &service
		}
	}
	if foundService == nil {
		return "", fmt.Errorf("no service '%s' found in namespace '%s'", upstreamService, namespace)
	}

	port := foundService.Spec.Ports[0].Port
	return fmt.Sprintf("http://%s:%d", foundService.Name, port), nil
}