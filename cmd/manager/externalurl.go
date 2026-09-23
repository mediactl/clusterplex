package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// externalURL is the address Plex advertises to clients as a custom
// connection. An explicit plex-external-url wins; otherwise it is read from
// the LoadBalancer address of the Service named by plex-external-service.
//
// Reading it beats writing it down. An address written into a manifest is
// correct only until the LoadBalancer changes, and a stale one is not a
// failure anybody sees: it passes validation, and Plex starts and serves. It
// breaks the moment a client signs in, because plex.tv hands back the
// published connection and the client switches to it.
//
// An empty result is not an error. Enforced then leaves customConnections
// alone, keeping whatever Plex already advertises, and this is resolved again
// before the next start — so a LoadBalancer that has not been given an
// address yet costs a restart rather than a wrong advertisement.
func (m *Manager) externalURL(ctx context.Context) string {
	if m.Config.ExternalURL != "" {
		return m.Config.ExternalURL
	}
	if m.Config.ExternalService == "" {
		return ""
	}

	svc, err := m.K8sClient.CoreV1().Services(m.Config.Namespace).
		Get(ctx, m.Config.ExternalService, metav1.GetOptions{})
	if err != nil {
		m.Logger.Warn("cannot read the Service Plex advertises; leaving the advertisement as it is",
			"service", m.Config.ExternalService, "error", err)
		return ""
	}

	host := ""
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		// An address is preferred over a name: it is what the Service has,
		// rather than something that has to resolve to it.
		if ing.IP != "" {
			host = ing.IP
			break
		}
		if ing.Hostname != "" && host == "" {
			host = ing.Hostname
		}
	}
	if host == "" {
		m.Logger.Warn("the Service Plex advertises has no external address yet",
			"service", m.Config.ExternalService)
		return ""
	}
	if len(svc.Spec.Ports) == 0 {
		return ""
	}

	// The Service's port, not Plex's: a client reaches Plex on whatever the
	// LoadBalancer publishes, which need not be the port Plex binds.
	//
	// The scheme is http because that is what the Service carries. plex.tv may
	// still publish it as an https plex.direct URI, which works because the
	// path is L4 and Plex terminates its own TLS; see docs/configuration.md.
	return fmt.Sprintf("http://%s:%d", host, svc.Spec.Ports[0].Port)
}
