package kubernetes

import (
	"context"
	"errors"
	"io/ioutil"
	"log"
	"strings"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/command/agent/auth"
)

const (
	serviceAccountFile = "var/run/secrets/kubernetes.io/serviceaccount/token"
)

type kubeMethod struct {
	logger    hclog.Logger
	mountPath string

	role string
}

func NewKubernetesAuthMethod(conf *auth.AuthConfig) (auth.AuthMethod, error) {
	if conf == nil {
		return nil, errors.New("empty config")
	}
	if conf.Config == nil {
		return nil, errors.New("empty config data")
	}

	k := &kubeMethod{
		logger:    conf.Logger,
		mountPath: conf.MountPath,
	}

	roleRaw, ok := conf.Config["role"]
	if !ok {
		return nil, errors.New("missing 'role' value")
	}
	k.role, ok = roleRaw.(string)
	if !ok {
		return nil, errors.New("could not convert 'role' config value to string")
	}
	if k.role == "" {
		return nil, errors.New("'role' value is empty")
	}

	return k, nil
}

func (k *kubeMethod) Authenticate(ctx context.Context, client *api.Client) (*api.Secret, error) {
	k.logger.Trace("beginning authentication")
	content, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		log.Fatal(err)
	}

	secret, err := client.Logical().Write("/auth/kubernetes/login", map[string]interface{}{
		"role": k.role,
		"jwt":  strings.TrimSpace(string(content)),
	})
	if err != nil {
		return nil, errwrap.Wrapf("error authenticating with Kubernetes: {{err}}", err)
	}

	return secret, nil
}

func (k *kubeMethod) NewCreds() chan struct{} {
	return nil
}

func (k *kubeMethod) Shutdown() {
}