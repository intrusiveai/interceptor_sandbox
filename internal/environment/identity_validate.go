package environment

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

var reservedIdentityClaims = map[string]struct{}{
	"iss": {}, "sub": {}, "aud": {}, "exp": {}, "iat": {}, "nbf": {},
	"jti": {}, "scope": {}, "client_id": {}, "azp": {}, "act": {},
}

type identityValidationResult struct {
	issuerIDs    map[string]IdentityIssuer
	principals   map[string]IdentityPrincipal
	serviceAuth  map[string]ServiceAuthorization
	deliveryEnvs map[string]string
}

func validateIdentity(blueprint *Blueprint, serviceIDs map[string]struct{}, hostnames, listens map[string]string) (identityValidationResult, []error) {
	result := identityValidationResult{
		issuerIDs: map[string]IdentityIssuer{}, principals: map[string]IdentityPrincipal{},
		serviceAuth: map[string]ServiceAuthorization{}, deliveryEnvs: map[string]string{},
	}
	var failures []error
	add := func(err error) {
		if err != nil {
			failures = append(failures, err)
		}
	}
	serviceDefinitions := map[string]Service{}
	for _, service := range blueprint.Services {
		serviceDefinitions[service.ID] = service
	}

	issuerSeen := map[string]struct{}{}
	for index, issuer := range blueprint.Identity.Issuers {
		prefix := fmt.Sprintf("identity.issuers[%d]", index)
		add(validateID(prefix+".id", issuer.ID, issuerSeen))
		if _, exists := serviceIDs[issuer.ID]; exists {
			add(fmt.Errorf("%s.id %q conflicts with a synthetic service", prefix, issuer.ID))
		}
		if issuer.Kind != "oidc" {
			add(fmt.Errorf("%s.kind %q is unsupported", prefix, issuer.Kind))
		}
		add(validateListen(prefix+".listen", issuer.Listen, listens, issuer.ID))
		add(validateHostnames(prefix, issuer.Hostnames, hostnames, issuer.ID))
		add(validateIssuerURL(prefix+".issuer_url", blueprint, issuer))
		if issuer.AccessTokenTTLSeconds < 0 || issuer.AccessTokenTTLSeconds > 86400 {
			add(fmt.Errorf("%s.access_token_ttl_seconds must be between 1 and 86400 when set", prefix))
		}
		result.issuerIDs[issuer.ID] = issuer
	}

	principalSeen := map[string]struct{}{}
	clientIDs := map[string]string{}
	for index, principal := range blueprint.Identity.Principals {
		prefix := fmt.Sprintf("identity.principals[%d]", index)
		add(validateID(prefix+".id", principal.ID, principalSeen))
		if principal.Kind != "agent" && principal.Kind != "user" && principal.Kind != "service" {
			add(fmt.Errorf("%s.kind %q is unsupported", prefix, principal.Kind))
		}
		if _, exists := result.issuerIDs[principal.Issuer]; !exists {
			add(fmt.Errorf("%s.issuer references unknown issuer %q", prefix, principal.Issuer))
		}
		if principal.ClientID == "" && principal.ClientSecret != "" {
			add(fmt.Errorf("%s.client_id is required when client_secret is set", prefix))
		}
		if principal.ClientID != "" {
			if previous, exists := clientIDs[principal.ClientID]; exists {
				add(fmt.Errorf("%s.client_id %q duplicates principal %q", prefix, principal.ClientID, previous))
			}
			clientIDs[principal.ClientID] = principal.ID
		}
		if principal.Kind == "user" && (principal.ClientID != "" || principal.ClientSecret != "") {
			add(fmt.Errorf("%s user principals may not declare OAuth client credentials", prefix))
		}
		for name := range principal.Claims {
			if _, reserved := reservedIdentityClaims[name]; reserved {
				add(fmt.Errorf("%s.claims may not override reserved claim %q", prefix, name))
			}
		}
		result.principals[principal.ID] = principal
	}

	grantSeen := map[string]struct{}{}
	for index, grant := range blueprint.Identity.Grants {
		prefix := fmt.Sprintf("identity.grants[%d]", index)
		add(validateID(prefix+".id", grant.ID, grantSeen))
		principal, principalExists := result.principals[grant.Principal]
		if !principalExists {
			add(fmt.Errorf("%s.principal references unknown principal %q", prefix, grant.Principal))
		}
		service, serviceExists := serviceDefinitions[grant.Service]
		if !serviceExists {
			add(fmt.Errorf("%s.service references unknown service %q", prefix, grant.Service))
		} else if service.Authorization == nil {
			add(fmt.Errorf("%s.service %q does not declare authorization", prefix, grant.Service))
		} else if principalExists && service.Authorization.Issuer != principal.Issuer {
			add(fmt.Errorf("%s principal and protected service must use the same issuer", prefix))
		}
		add(validateScopes(prefix+".scopes", grant.Scopes))
	}

	delegationSeen := map[string]struct{}{}
	for index, delegation := range blueprint.Identity.Delegations {
		prefix := fmt.Sprintf("identity.delegations[%d]", index)
		add(validateID(prefix+".id", delegation.ID, delegationSeen))
		agent, agentExists := result.principals[delegation.Agent]
		user, userExists := result.principals[delegation.User]
		if !agentExists || agent.Kind != "agent" {
			add(fmt.Errorf("%s.agent must reference an agent principal", prefix))
		}
		if !userExists || user.Kind != "user" {
			add(fmt.Errorf("%s.user must reference a user principal", prefix))
		}
		if agentExists && userExists && agent.Issuer != user.Issuer {
			add(fmt.Errorf("%s agent and user must use the same synthetic issuer", prefix))
		}
		service, serviceExists := serviceDefinitions[delegation.Service]
		if !serviceExists {
			add(fmt.Errorf("%s.service references unknown service %q", prefix, delegation.Service))
		} else if service.Authorization == nil {
			add(fmt.Errorf("%s.service %q does not declare authorization", prefix, delegation.Service))
		} else if agentExists && service.Authorization.Issuer != agent.Issuer {
			add(fmt.Errorf("%s principals and protected service must use the same issuer", prefix))
		}
		add(validateScopes(prefix+".scopes", delegation.Scopes))
	}

	for index, service := range blueprint.Services {
		if service.Authorization == nil {
			continue
		}
		prefix := fmt.Sprintf("services[%d].authorization", index)
		if _, exists := result.issuerIDs[service.Authorization.Issuer]; !exists {
			add(fmt.Errorf("%s.issuer references unknown issuer %q", prefix, service.Authorization.Issuer))
		}
		if strings.TrimSpace(service.Authorization.Audience) == "" {
			add(fmt.Errorf("%s.audience is required", prefix))
		}
		add(validateScopes(prefix+".required_scopes", service.Authorization.RequiredScopes))
		result.serviceAuth[service.ID] = *service.Authorization
	}

	deliverySeen := map[string]struct{}{}
	for index, delivery := range blueprint.Identity.Deliveries {
		prefix := fmt.Sprintf("identity.deliveries[%d]", index)
		add(validateID(prefix+".id", delivery.ID, deliverySeen))
		issuer, issuerExists := result.issuerIDs[delivery.Issuer]
		principal, principalExists := result.principals[delivery.Principal]
		if !issuerExists {
			add(fmt.Errorf("%s.issuer references unknown issuer %q", prefix, delivery.Issuer))
		}
		if !principalExists {
			add(fmt.Errorf("%s.principal references unknown principal %q", prefix, delivery.Principal))
		} else if issuerExists && principal.Issuer != issuer.ID {
			add(fmt.Errorf("%s principal is not owned by issuer %q", prefix, issuer.ID))
		}
		switch delivery.Kind {
		case "oauth_client":
			if !principalExists || principal.ClientID == "" || principal.ClientSecret == "" {
				add(fmt.Errorf("%s requires a principal with client_id and client_secret", prefix))
			}
			add(validateDeliveryEnv(prefix+".client_id_environment", delivery.ClientIDEnvironment, result.deliveryEnvs, delivery.ID))
			add(validateDeliveryEnv(prefix+".client_secret_environment", delivery.ClientSecretEnvironment, result.deliveryEnvs, delivery.ID))
			add(validateDeliveryEnv(prefix+".token_url_environment", delivery.TokenURLEnvironment, result.deliveryEnvs, delivery.ID))
			if delivery.Path != "" || delivery.EnvironmentVariable != "" || delivery.EndpointEnvironment != "" || delivery.AuthorizationTokenEnvironment != "" {
				add(fmt.Errorf("%s oauth_client contains fields for another delivery kind", prefix))
			}
		case "projected_jwt":
			clean, err := validateContainerPath(prefix+".path", delivery.Path)
			add(err)
			if err == nil && !strings.HasPrefix(clean, "/run/interceptor-session/identity/") {
				add(fmt.Errorf("%s.path must be beneath /run/interceptor-session/identity", prefix))
			}
			if strings.TrimSpace(delivery.Audience) == "" {
				add(fmt.Errorf("%s.audience is required", prefix))
			}
			add(validateDeliveryEnv(prefix+".environment_variable", delivery.EnvironmentVariable, result.deliveryEnvs, delivery.ID))
			if delivery.ClientIDEnvironment != "" || delivery.ClientSecretEnvironment != "" || delivery.TokenURLEnvironment != "" || delivery.EndpointEnvironment != "" || delivery.AuthorizationTokenEnvironment != "" {
				add(fmt.Errorf("%s projected_jwt contains fields for another delivery kind", prefix))
			}
		case "aws_container_credentials":
			if principalExists && principal.Kind == "user" {
				add(fmt.Errorf("%s requires an agent or service principal", prefix))
			}
			add(validateDeliveryEnv(prefix+".endpoint_environment", delivery.EndpointEnvironment, result.deliveryEnvs, delivery.ID))
			if delivery.AuthorizationTokenEnvironment != "" {
				add(validateDeliveryEnv(prefix+".authorization_token_environment", delivery.AuthorizationTokenEnvironment, result.deliveryEnvs, delivery.ID))
			}
			if delivery.Path != "" || delivery.EnvironmentVariable != "" || delivery.ClientIDEnvironment != "" || delivery.ClientSecretEnvironment != "" || delivery.TokenURLEnvironment != "" {
				add(fmt.Errorf("%s aws_container_credentials contains fields for another delivery kind", prefix))
			}
		default:
			add(fmt.Errorf("%s.kind %q is unsupported", prefix, delivery.Kind))
		}
	}

	if inbound := blueprint.Identity.Inbound; inbound != nil {
		if _, exists := result.issuerIDs[inbound.Issuer]; !exists {
			add(fmt.Errorf("identity.inbound.issuer references unknown issuer %q", inbound.Issuer))
		}
		audiencePrincipal, audienceExists := result.principals[inbound.Audience]
		if !audienceExists || audiencePrincipal.Kind != "agent" || audiencePrincipal.Issuer != inbound.Issuer {
			add(errors.New("identity.inbound.audience must reference an agent principal owned by its issuer"))
		}
		if inbound.ApplicationHeader == "" {
			inbound.ApplicationHeader = "Authorization"
		}
		if blueprint.Application.Interface.Adapter != "http" {
			add(errors.New("identity.inbound currently requires an HTTP application interface"))
		}
		if http.CanonicalHeaderKey(inbound.ApplicationHeader) != "Authorization" {
			add(errors.New("identity.inbound.application_header currently supports only Authorization"))
		}
	}

	for name, owner := range result.deliveryEnvs {
		if _, exists := blueprint.Application.Launch.Environment[name]; exists {
			add(fmt.Errorf("identity delivery %q environment variable %q conflicts with application.launch.environment", owner, name))
		}
		for _, credential := range blueprint.Credentials {
			if credential.EnvironmentVariable == name {
				add(fmt.Errorf("identity delivery %q environment variable %q conflicts with credential %q", owner, name, credential.ID))
			}
		}
	}

	return result, failures
}

func validateIssuerURL(name string, blueprint *Blueprint, issuer IdentityIssuer) error {
	parsed, err := url.Parse(issuer.IssuerURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return fmt.Errorf("%s must be an absolute HTTP or HTTPS origin without a path, query, user info, or fragment", name)
	}
	if parsed.Scheme == "https" && !blueprint.Network.TLS.Enabled {
		return fmt.Errorf("%s uses HTTPS but network.tls is not enabled", name)
	}
	host := strings.ToLower(parsed.Hostname())
	declaredHost := false
	for _, hostname := range issuer.Hostnames {
		if strings.EqualFold(hostname, host) {
			declaredHost = true
			break
		}
	}
	if !isLoopbackHost(host) && !declaredHost {
		return fmt.Errorf("%s host must be loopback or one of the issuer hostnames", name)
	}
	if parsed.Scheme == "https" && !declaredHost {
		return fmt.Errorf("%s HTTPS host must be one of the issuer hostnames", name)
	}
	_, listenPort, listenErr := net.SplitHostPort(issuer.Listen)
	urlPort := parsed.Port()
	if urlPort == "" {
		if parsed.Scheme == "https" {
			urlPort = "443"
		} else {
			urlPort = "80"
		}
	}
	frontDoorPort := ""
	if parsed.Scheme == "https" && blueprint.Network.TLS.Enabled {
		_, frontDoorPort, _ = net.SplitHostPort(blueprint.Network.TLS.Listen)
	} else if parsed.Scheme == "http" && blueprint.Network.HTTP.Enabled {
		_, frontDoorPort, _ = net.SplitHostPort(blueprint.Network.HTTP.Listen)
	}
	if listenErr == nil && urlPort != listenPort && urlPort != frontDoorPort {
		return fmt.Errorf("%s port must match issuer listen port %s", name, listenPort)
	}
	return nil
}

func validateScopes(name string, scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%s must not be empty", name)
	}
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		if strings.TrimSpace(scope) != scope || scope == "" || strings.ContainsAny(scope, "\x00\r\n\t ") {
			return fmt.Errorf("%s contains invalid scope %q", name, scope)
		}
		if _, exists := seen[scope]; exists {
			return fmt.Errorf("%s contains duplicate scope %q", name, scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func validateDeliveryEnv(name, value string, seen map[string]string, owner string) error {
	if !environmentName.MatchString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	if previous, exists := seen[value]; exists {
		return fmt.Errorf("%s conflicts with identity delivery %q", name, previous)
	}
	seen[value] = owner
	return nil
}
