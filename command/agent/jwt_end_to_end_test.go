package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/ioutil"
	"os"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	vaultjwt "github.com/hashicorp/vault-plugin-auth-jwt"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/command/agent/auth"
	agentjwt "github.com/hashicorp/vault/command/agent/auth/jwt"
	"github.com/hashicorp/vault/command/agent/sink"
	"github.com/hashicorp/vault/command/agent/sink/file"
	"github.com/hashicorp/vault/helper/dhutil"
	"github.com/hashicorp/vault/helper/jsonutil"
	"github.com/hashicorp/vault/helper/logging"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/vault"
	jose "gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

func getTestJWT(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	cl := jwt.Claims{
		Subject:   "r3qXcK2bix9eFECzsU3Sbmh0K16fatW6@clients",
		Issuer:    "https://team-vault.auth0.com/",
		NotBefore: jwt.NewNumericDate(time.Now().Add(-5 * time.Second)),
		Audience:  jwt.Audience{"https://vault.plugin.auth.jwt.test"},
	}

	privateCl := struct {
		User   string   `json:"https://vault/user"`
		Groups []string `json:"https://vault/groups"`
	}{
		"jeff",
		[]string{"foo", "bar"},
	}

	var key *ecdsa.PrivateKey
	block, _ := pem.Decode([]byte(ecdsaPrivKey))
	if block != nil {
		var err error
		key, err = x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
	}

	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := jwt.Signed(sig).Claims(cl).Claims(privateCl).CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}

	return raw, key
}

func TestJWTEndtoEnd(t *testing.T) {
	logger := logging.NewVaultLogger(hclog.Trace)
	coreConfig := &vault.CoreConfig{
		Logger: logger,
		CredentialBackends: map[string]logical.Factory{
			"jwt": vaultjwt.Factory,
		},
	}
	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
	})
	cluster.Start()
	defer cluster.Cleanup()

	vault.TestWaitActive(t, cluster.Cores[0].Core)
	client := cluster.Cores[0].Client

	// Setup Vault
	err := client.Sys().EnableAuthWithOptions("jwt", &api.EnableAuthOptions{
		Type: "jwt",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("auth/jwt/config", map[string]interface{}{
		"bound_issuer":           "https://team-vault.auth0.com/",
		"jwt_validation_pubkeys": ecdsaPubKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("auth/jwt/role/test", map[string]interface{}{
		"bound_subject":   "r3qXcK2bix9eFECzsU3Sbmh0K16fatW6@clients",
		"bound_audiences": "https://vault.plugin.auth.jwt.test",
		"user_claim":      "https://vault/user",
		"groups_claim":    "https://vault/groups",
		"policies":        "test",
		"period":          "3s",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Generate encryption params
	pub, pri, err := dhutil.GeneratePublicPrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	// We close these right away because we're just basically testing
	// permissions and finding a usable file name
	inf, err := ioutil.TempFile("", "auth.jwt.test.")
	if err != nil {
		t.Fatal(err)
	}
	in := inf.Name()
	inf.Close()
	os.Remove(in)
	t.Logf("input: %s", in)

	ouf, err := ioutil.TempFile("", "auth.tokensink.test.")
	if err != nil {
		t.Fatal(err)
	}
	out := ouf.Name()
	ouf.Close()
	os.Remove(out)
	t.Logf("output: %s", out)

	dhpathf, err := ioutil.TempFile("", "auth.dhpath.test.")
	if err != nil {
		t.Fatal(err)
	}
	dhpath := dhpathf.Name()
	dhpathf.Close()
	os.Remove(dhpath)
	t.Logf("dhpath: %s", dhpath)

	// Write DH public key to file
	mPubKey, err := jsonutil.EncodeJSON(&dhutil.PublicKeyInfo{
		Curve25519PublicKey: pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(dhpath, mPubKey, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancelFunc := context.WithCancel(context.Background())

	am, err := agentjwt.NewJWTAuthMethod(&auth.AuthConfig{
		Logger:    logger.Named("auth.jwt"),
		MountPath: "auth/jwt",
		Config: map[string]interface{}{
			"path": in,
			"role": "test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ah := auth.NewAuthHandler(&auth.AuthHandlerConfig{
		Logger:  logger.Named("auth.handler"),
		Client:  client,
		Context: ctx,
	})
	go ah.Run(am)
	defer func() {
		<-ah.DoneCh
	}()

	fs, err := file.NewFileSink(&sink.SinkConfig{
		Logger:  logger.Named("sink.file"),
		WrapTTL: 10 * time.Second,
		AAD:     "foobar",
		DHType:  "curve25519",
		DHPath:  dhpath,
		Config: map[string]interface{}{
			"path": out,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ss := sink.NewSinkServer(&sink.SinkServerConfig{
		Logger:  logger.Named("sink.server"),
		Client:  client,
		Context: ctx,
	})
	go ss.Run(ah.OutputCh, []sink.Sink{fs})
	defer func() {
		<-ss.DoneCh
	}()

	// This has to be after the other defers so it happens first
	defer cancelFunc()

	// Check that no jwt file exists
	_, err = os.Lstat(in)
	if err == nil {
		t.Fatal("expected err")
	}
	if !os.IsNotExist(err) {
		t.Fatal("expected notexist err")
	}
	_, err = os.Lstat(out)
	if err == nil {
		t.Fatal("expected err")
	}
	if !os.IsNotExist(err) {
		t.Fatal("expected notexist err")
	}

	cloned, err := client.Clone()
	if err != nil {
		t.Fatal(err)
	}

	// Get a token
	jwtToken, _ := getTestJWT(t)
	if err := ioutil.WriteFile(in, []byte(jwtToken), 0600); err != nil {
		t.Fatal(err)
	}

	checkToken := func() string {
		timeout := time.Now().Add(5 * time.Second)
		for {
			if time.Now().After(timeout) {
				t.Fatal("did not find a written token after timeout")
			}
			val, err := ioutil.ReadFile(out)
			if err == nil {
				os.Remove(out)
				if len(val) == 0 {
					t.Fatal("written token was empty")
				}

				// First decrypt it
				resp := new(dhutil.Envelope)
				if err := jsonutil.DecodeJSON(val, resp); err != nil {
					continue
				}

				aesKey, err := dhutil.GenerateSharedKey(pri, resp.Curve25519PublicKey)
				if err != nil {
					t.Fatal(err)
				}
				if len(aesKey) == 0 {
					t.Fatal("got empty aes key")
				}

				val, err = dhutil.DecryptAES(aesKey, resp.EncryptedPayload, resp.Nonce, []byte("foobar"))
				if err != nil {
					t.Fatalf("error: %v\nresp: %v", err, string(val))
				}

				// Now unwrap it
				wrapInfo := new(api.SecretWrapInfo)
				if err := jsonutil.DecodeJSON(val, wrapInfo); err != nil {
					t.Fatal(err)
				}
				switch {
				case wrapInfo.TTL != 10:
					t.Fatalf("bad wrap info: %v", wrapInfo.TTL)
				case wrapInfo.CreationPath != "sys/wrapping/wrap":
					t.Fatalf("bad wrap path: %v", wrapInfo.CreationPath)
				case wrapInfo.Token == "":
					t.Fatal("wrap token is empty")
				}
				cloned.SetToken(wrapInfo.Token)
				secret, err := cloned.Logical().Unwrap("")
				switch {
				case err != nil:
					t.Fatal(err)
				case secret.Data == nil:
					t.Fatal("unwrap secret data is nil")
				case secret.Data["token"] == nil:
					t.Fatal("unwrap secret data is nil")
				}

				return secret.Data["token"].(string)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	origToken := checkToken()

	// Period of 3 seconds, so should still be alive after 7
	timeout := time.Now().Add(7 * time.Second)
	cloned.SetToken(origToken)
	for {
		if time.Now().After(timeout) {
			break
		}
		secret, err := cloned.Auth().Token().LookupSelf()
		if err != nil {
			t.Fatal(err)
		}
		ttl, err := secret.Data["ttl"].(json.Number).Int64()
		if err != nil {
			t.Fatal(err)
		}
		if ttl > 3 {
			t.Fatalf("unexpected ttl: %v", secret.Data["ttl"])
		}
	}

	// Get another token to test the backend pushing the need to authenticate
	// to the handler
	jwtToken, _ = getTestJWT(t)
	if err := ioutil.WriteFile(in, []byte(jwtToken), 0600); err != nil {
		t.Fatal(err)
	}

	newToken := checkToken()
	if newToken == origToken {
		t.Fatal("found same token written")
	}

	// Repeat the period test. At the end the old token should have expired and
	// the new token should still be alive after 7
	timeout = time.Now().Add(7 * time.Second)
	cloned.SetToken(newToken)
	for {
		if time.Now().After(timeout) {
			break
		}
		secret, err := cloned.Auth().Token().LookupSelf()
		if err != nil {
			t.Fatal(err)
		}
		ttl, err := secret.Data["ttl"].(json.Number).Int64()
		if err != nil {
			t.Fatal(err)
		}
		if ttl > 3 {
			t.Fatalf("unexpected ttl: %v", secret.Data["ttl"])
		}
	}

	cloned.SetToken(origToken)
	_, err = cloned.Auth().Token().LookupSelf()
	if err == nil {
		t.Fatal("expected error")
	}
}

const (
	ecdsaPrivKey string = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIKfldwWLPYsHjRL9EVTsjSbzTtcGRu6icohNfIqcb6A+oAoGCCqGSM49
AwEHoUQDQgAE4+SFvPwOy0miy/FiTT05HnwjpEbSq+7+1q9BFxAkzjgKnlkXk5qx
hzXQvRmS4w9ZsskoTZtuUI+XX7conJhzCQ==
-----END EC PRIVATE KEY-----`

	ecdsaPubKey string = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE4+SFvPwOy0miy/FiTT05HnwjpEbS
q+7+1q9BFxAkzjgKnlkXk5qxhzXQvRmS4w9ZsskoTZtuUI+XX7conJhzCQ==
-----END PUBLIC KEY-----`
)
