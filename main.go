package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"flag"
	"net/url"

	reg "github.com/appscode/docker-registry-client/registry"
	manifestV1 "github.com/docker/distribution/manifest/schema1"
	manifestV2 "github.com/docker/distribution/manifest/schema2"
	"github.com/golang/glog"
	"github.com/moul/http2curl"
	"k8s.io/api/core/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/kubernetes/pkg/credentialprovider"
	// Credential providers
	"github.com/appscode/kutil/meta"
	_ "k8s.io/kubernetes/pkg/credentialprovider/aws"
	_ "k8s.io/kubernetes/pkg/credentialprovider/azure"
	_ "k8s.io/kubernetes/pkg/credentialprovider/gcp"
	// _ "k8s.io/kubernetes/pkg/credentialprovider/rancher" // enable in Kube 1.10
	"net/http/httputil"

	"k8s.io/kubernetes/pkg/util/parsers"
)

// nginx
// appscode/voyager:6.0.0
// tigerworks/labels
// k8s.gcr.io/kube-proxy-amd64:v1.10.0
// gcr.io/tigerworks-kube/docker-image-puller:latest
// appscode/docker-image-puller@sha256:a54f1be7edda4305e59544ef4014494206245be08422258d6677ff273223c5a8
// "tigerworks/nginx:1.13"
func main() {
	var (
		img            string = "appscode/voyager:6.0.0"
		masterURL      string
		kubeconfigPath string
	)
	if !meta.PossiblyInCluster() {
		kubeconfigPath = filepath.Join(homedir.HomeDir(), ".kube/config")
	}

	flag.StringVar(&img, "image", img, "Name of docker image as used in a Kubernetes container")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	flag.StringVar(&kubeconfigPath, "kubeconfig", kubeconfigPath, "Path to kubeconfig file")
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfigPath)
	if err != nil {
		glog.Fatalf("Could not get Kubernetes config: %s", err)
	}

	kc := kubernetes.NewForConfigOrDie(config)

	secrets, err := kc.CoreV1().Secrets(metav1.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		glog.Fatalln(err)
	}

	var pullSecrets []v1.Secret
	for _, sec := range secrets.Items {
		if sec.Type == core.SecretTypeDockerConfigJson || sec.Type == core.SecretTypeDockercfg {
			pullSecrets = append(pullSecrets, sec)
		}
	}

	mf2, err := PullImage(img, pullSecrets)
	if err != nil {
		glog.Fatalln(err)
	}
	switch manifest := mf2.(type) {
	case *manifestV2.DeserializedManifest:
		data, _ := manifest.MarshalJSON()
		fmt.Println("V2 Manifest:", string(data))
	case *manifestV1.SignedManifest:
		data, _ := manifest.MarshalJSON()
		fmt.Println("V1 Manifest:", string(data))
	}
}

// ref: https://github.com/kubernetes/kubernetes/blob/release-1.9/pkg/kubelet/kuberuntime/kuberuntime_image.go#L29

// PullImage pulls an image from the network to local storage using the supplied secrets if necessary.
func PullImage(img string, pullSecrets []v1.Secret) (interface{}, error) {
	repoToPull, tag, digest, err := parsers.ParseImageName(img)
	if err != nil {
		return nil, err
	}

	parts := strings.SplitN(repoToPull, "/", 2)
	regURL := parts[0]
	repo := parts[1]
	fmt.Println(regURL, repo, tag, digest)
	ref := tag
	if ref == "" {
		ref = digest
	}

	if strings.HasPrefix(regURL, "docker.io") || strings.HasPrefix(regURL, "index.docker.io") {
		regURL = "registry-1.docker.io"
	}
	if !strings.HasPrefix(regURL, "https://") && !strings.HasPrefix(regURL, "http://") {
		regURL = "https://" + regURL
	}
	_, err = url.Parse(regURL)
	if err != nil {
		return nil, err
	}

	keyring, err := credentialprovider.MakeDockerKeyring(pullSecrets, credentialprovider.NewDockerKeyring())
	if err != nil {
		return nil, err
	}

	creds, withCredentials := keyring.Lookup(repoToPull)
	if !withCredentials {
		glog.V(3).Infof("Pulling image %q without credentials", img)
		return PullManifest(repo, ref, &AuthConfig{ServerAddress: regURL})
	}

	var pullErrs []error
	for _, currentCreds := range creds {
		authConfig := credentialprovider.LazyProvide(currentCreds)
		auth := &AuthConfig{
			Username:      authConfig.Username,
			Password:      authConfig.Password,
			Auth:          authConfig.Auth,
			ServerAddress: authConfig.ServerAddress,
		}
		if auth.ServerAddress == "" {
			auth.ServerAddress = regURL
		}

		mf, err := PullManifest(repo, ref, auth)
		if err == nil {
			return mf, nil
		}
		pullErrs = append(pullErrs, err)
	}
	return nil, utilerrors.NewAggregate(pullErrs)
}

func PullManifest(repo, ref string, auth *AuthConfig) (interface{}, error) {
	hub := &reg.Registry{
		URL: auth.ServerAddress,
		Client: &http.Client{
			Transport: reg.WrapTransport(CC(http.DefaultTransport), auth.ServerAddress, auth.Username, auth.Password),
		},
		Logf: reg.Log,
	}
	return hub.ManifestVx(repo, ref)
}

// AuthConfig contains authorization information for connecting to a registry.
type AuthConfig struct {
	Username      string
	Password      string
	Auth          string
	ServerAddress string
}

func CC(t http.RoundTripper) http.RoundTripper {
	return &logTransport{Transport: t}
}

type logTransport struct {
	Transport http.RoundTripper
}

func (t *logTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cmd, _ := http2curl.GetCurlCommand(request)
	fmt.Println(cmd)
	if glog.V(10) {
		cmd, _ := http2curl.GetCurlCommand(request)
		glog.Infoln("request:", cmd)
	}
	resp, err := t.Transport.RoundTrip(request)
	if err == nil {
		b, err := httputil.DumpResponse(resp, true)
		if err == nil {
			fmt.Println(string(b))
		}
	}
	return resp, err
}
