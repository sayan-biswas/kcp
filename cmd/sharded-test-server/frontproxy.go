/*
Copyright 2022 The KCP Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/abiosoft/lineprefix"
	"github.com/fatih/color"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/kcp/cmd/sharded-test-server/third_party/library-go/crypto"
	"github.com/kcp-dev/kcp/cmd/test-server/helpers"
	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	"github.com/kcp-dev/kcp/test/e2e/framework"
)

func startFrontProxy(ctx context.Context, args []string, servingCA *crypto.CA, hostIP string) error {
	blue := color.New(color.BgGreen, color.FgBlack).SprintFunc()
	inverse := color.New(color.BgHiWhite, color.FgGreen).SprintFunc()
	out := lineprefix.New(
		lineprefix.Prefix(blue(" PROXY ")),
		lineprefix.Color(color.New(color.FgHiGreen)),
	)
	successOut := lineprefix.New(
		lineprefix.Prefix(inverse(" PROXY ")),
		lineprefix.Color(color.New(color.FgHiWhite)),
	)

	if err := ioutil.WriteFile(".kcp-front-proxy/mapping.yaml", []byte(`
- path: /services/
  backend: https://localhost:6444
  backend_server_ca: .kcp/serving-ca.crt
  proxy_client_cert: .kcp-front-proxy/requestheader.crt
  proxy_client_key: .kcp-front-proxy/requestheader.key
- path: /clusters/
  backend: https://localhost:6444
  backend_server_ca: .kcp/serving-ca.crt
  proxy_client_cert: .kcp-front-proxy/requestheader.crt
  proxy_client_key: .kcp-front-proxy/requestheader.key
`), 0644); err != nil {
		return fmt.Errorf("failed to create front-proxy mapping.yaml: %w\n", err)
	}

	// write root shard kubeconfig
	configLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: ".kcp-0/admin.kubeconfig"}, nil)
	raw, err := configLoader.RawConfig()
	if err != nil {
		return err
	}
	raw.CurrentContext = "system:admin"
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return err
	}
	if err := clientcmd.WriteToFile(raw, ".kcp/root.kubeconfig"); err != nil {
		return err
	}

	// create serving cert
	hostnames := sets.NewString("localhost", hostIP)
	klog.Infof("Creating kcp-front-proxy serving cert with hostnames %v", hostnames)
	cert, err := servingCA.MakeServerCert(hostnames, 365)
	if err != nil {
		return fmt.Errorf("failed to create server cert: %w\n", err)
	}
	if err := cert.WriteCertConfigFile(".kcp-front-proxy/apiserver.crt", ".kcp-front-proxy/apiserver.key"); err != nil {
		return fmt.Errorf("failed to write server cert: %w\n", err)
	}

	// run front-proxy command
	commandLine := append(framework.DirectOrGoRunCommand("kcp-front-proxy"),
		"--mapping-file=.kcp-front-proxy/mapping.yaml",
		"--root-directory=.kcp-front-proxy",
		"--root-kubeconfig=.kcp/root.kubeconfig",
		"--client-ca-file=.kcp/client-ca.crt",
		"--tls-cert-file=.kcp-front-proxy/apiserver.crt",
		"--tls-private-key-file=.kcp-front-proxy/apiserver.key",
		"--secure-port=6443",
	)
	commandLine = append(commandLine, args...)
	fmt.Fprintf(out, "running: %v\n", strings.Join(commandLine, " ")) // nolint: errcheck

	cmd := exec.CommandContext(ctx, commandLine[0], commandLine[1:]...)

	logDir := flag.Lookup("log-dir-path").Value.String()
	if err != nil {
		return err
	}
	logFilePath := ".kcp-front-proxy/proxy.log"
	if logDir != "" {
		logFilePath = filepath.Join(logDir, "kcp-front-proxy.log")
	}

	logFile, err := os.OpenFile(logFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	writer := helpers.NewHeadWriter(logFile, out)
	cmd.Stdout = writer
	cmd.Stdin = os.Stdin
	cmd.Stderr = writer

	if err := cmd.Start(); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		cmd.Process.Kill() // nolint: errcheck
	}()

	terminatedCh := make(chan int, 1)
	go func() {
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok { // nolint: errorlint
				terminatedCh <- exitErr.ExitCode()
			}
		} else {
			terminatedCh <- 0
		}
	}()

	// wait for readiness
	klog.Infof("Waiting for kcp-front-proxy to be up")
	for {
		time.Sleep(time.Second)

		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled")
		case rc := <-terminatedCh:
			return fmt.Errorf("kcp-front-proxy terminated with exit code %d", rc)
		default:
		}

		// intentionally load again every iteration because it can change
		configLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: ".kcp/admin.kubeconfig"},
			&clientcmd.ConfigOverrides{CurrentContext: "system:admin"},
		)
		config, err := configLoader.ClientConfig()
		if err != nil {
			continue
		}
		kcpClient, err := kcpclient.NewClusterForConfig(config)
		if err != nil {
			klog.Errorf("Failed to create kcp client: %v", err)
			continue
		}

		res := kcpClient.RESTClient().Get().AbsPath("/readyz").Do(ctx)
		if err := res.Error(); err != nil {
			klog.V(3).Infof("kcp-front-proxy not ready: %v", err)
		} else {
			var rc int
			res.StatusCode(&rc)
			if rc == http.StatusOK {
				break
			}
			if bs, err := res.Raw(); err != nil {
				klog.V(3).Infof("kcp-front-proxy not ready: %v", err)
			} else {
				klog.V(3).Infof("kcp-front-proxy not ready: http %d: %s", rc, string(bs))
			}
		}
	}
	if !klog.V(3).Enabled() {
		writer.StopOut()
	}
	fmt.Fprintf(successOut, "kcp-front-proxy is ready\n") // nolint: errcheck

	return nil
}

func kcpAdminKubeConfig(ctx context.Context, hostIP string) error {
	baseHost := fmt.Sprintf("https://%s:6443", hostIP)

	var kubeConfig clientcmdapi.Config
	kubeConfig.AuthInfos = map[string]*clientcmdapi.AuthInfo{
		"admin": {
			ClientKey:         ".kcp/kcp-admin.key",
			ClientCertificate: ".kcp/kcp-admin.crt",
		},
	}
	kubeConfig.Clusters = map[string]*clientcmdapi.Cluster{
		"root": {
			Server:               baseHost + "/clusters/root",
			CertificateAuthority: ".kcp/serving-ca.crt",
		},
		"root:default": {
			Server:               baseHost + "/clusters/root:default",
			CertificateAuthority: ".kcp/serving-ca.crt",
		},
		"system:admin": {
			Server:               baseHost,
			CertificateAuthority: ".kcp/serving-ca.crt",
		},
	}
	kubeConfig.Contexts = map[string]*clientcmdapi.Context{
		"root":         {Cluster: "root", AuthInfo: "admin"},
		"default":      {Cluster: "root:default", AuthInfo: "admin"},
		"system:admin": {Cluster: "system:admin", AuthInfo: "admin"},
	}
	kubeConfig.CurrentContext = "default"

	if err := clientcmdapi.FlattenConfig(&kubeConfig); err != nil {
		return err
	}

	return clientcmd.WriteToFile(kubeConfig, ".kcp/admin.kubeconfig")
}
