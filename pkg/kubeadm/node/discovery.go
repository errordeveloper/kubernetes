package kubenode

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/square/go-jose"
	clientcmdapi "k8s.io/kubernetes/pkg/client/unversioned/clientcmd/api"
	kubeadmapi "k8s.io/kubernetes/pkg/kubeadm/api"
)

func RetrieveTrustedClusterInfo(params *kubeadmapi.BootstrapParams) (*clientcmdapi.Config, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%v/api/v1alpha1/testclusterinfo", 8081), nil)
	if err != nil {
		return nil, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	io.Copy(buf, res.Body)
	res.Body.Close()

	object, err := jose.ParseSigned(buf.String())
	if err != nil {
		return nil, err
	}

	output, err := object.Verify([]byte(params.Discovery.BearerToken))
	if err != nil {
		return nil, err
	}

	fmt.Printf(string(output))

	caCert := []byte{}
	apiEndpoint := "https://localhost:443"
	return PerformTLSBootstrap(params, apiEndpoint, caCert)
}
