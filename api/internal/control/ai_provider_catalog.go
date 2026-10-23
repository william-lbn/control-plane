package control

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

type aiCatalogSnapshot struct {
	Config      *aigateway.Config
	Revision    int
	Unavailable map[string]bool
}

var errAIProviderCatalogChanged = errors.New("AI provider catalog changed before admission")

// Build a private immutable snapshot for each request. Never mutate s.ai while
// concurrent requests are using it. The Gateway opens boxes; API reads do not.
func (s *server) aiCatalog(ctx context.Context, open bool) (aiCatalogSnapshot, error) {
	return s.aiCatalogForOrganization(ctx, open, "")
}
func (s *server) aiCatalogForOrganization(ctx context.Context, open bool, org string) (aiCatalogSnapshot, error) {
	snapshot := aiCatalogSnapshot{Config: s.ai, Unavailable: map[string]bool{}}
	if s.ai == nil || s.aiProviders == nil {
		return snapshot, nil
	}
	copyConfig := *s.ai
	copyConfig.Routes = append([]aigateway.Route(nil), s.ai.Routes...)
	snapshot.Config = &copyConfig
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT revision FROM ai_provider_catalog_state WHERE singleton`).Scan(&snapshot.Revision); err != nil {
		return snapshot, err
	}
	if snapshot.Revision > 2000000000-copyConfig.Generation {
		return snapshot, errors.New("catalog generation exhausted")
	}
	copyConfig.Generation += snapshot.Revision
	rows, err := tx.Query(ctx, `SELECT provider_id,revision_id,base_url,endpoint_sha256,credential_mode,credential_box,network,policy_sha256,models,provider_type,COALESCE(org_id,'') FROM ai_provider_runtime WHERE org_id IS NULL OR org_id=NULLIF($1,'') ORDER BY provider_id`, org)
	if err != nil {
		return snapshot, err
	}
	seen := map[string]bool{}
	for _, route := range copyConfig.Routes {
		seen[route.ID] = true
	}
	for rows.Next() {
		var provider, revision, base, hash, mode, policy, kind, ownerOrg string
		var box *aigateway.SealedProviderCredential
		var network aigateway.ResolvedProviderEndpoint
		var models []aiProviderModel
		if err = rows.Scan(&provider, &revision, &base, &hash, &mode, &box, &network, &policy, &models, &kind, &ownerOrg); err != nil {
			break
		}
		// An operator policy change immediately withdraws older approvals; it never
		// turns an unapproved address into a network request or a fallback provider.
		if ownerOrg != "" && !s.aiProviders.policy.AllowOrganizationBYOK {
			continue
		}
		if policy != s.aiProviders.policySHA256 || network.BaseURL != base || network.EndpointSHA256 != hash || s.aiProviders.network.ValidateResolved(network) != nil {
			continue
		}
		secret := ""
		unavailable := false
		if open && mode != "none" {
			if s.aiProviders.opener == nil || box == nil {
				unavailable = true
			} else {
				secret, err = s.aiProviders.opener.Open(aigateway.ProviderCredentialScope{ProviderID: provider, RevisionID: revision, EndpointSHA256: hash}, *box)
				if err != nil {
					unavailable = true
					err = nil
				}
			}
		}
		for _, model := range models {
			if seen[model.ID] || len(copyConfig.Routes) >= 100 {
				rows.Close()
				return snapshot, errors.New("catalog aliases conflict")
			}
			seen[model.ID] = true
			route := aigateway.Route{OwnerOrgID: ownerOrg, ProviderID: provider, RevisionID: revision, ID: model.ID, Name: model.Name, Provider: "openai_compatible", ProviderType: kind, BaseURL: base, UpstreamModel: model.UpstreamModel, AllowedIPs: append([]string(nil), network.AllowedIPs...), LabHTTP: network.LabHTTP, MaxOutputTokens: model.MaxOutputTokens, InputModalities: append([]string(nil), model.InputModalities...), Protocols: append([]string(nil), model.Protocols...), Price: model.Price}
			if !unavailable {
				route, err = s.aiProviders.network.ConfigureRoute(network, route, secret)
				if err != nil {
					unavailable = true
					err = nil
				}
			}
			if unavailable {
				snapshot.Unavailable[model.ID] = true
			}
			copyConfig.Routes = append(copyConfig.Routes, route)
		}
		secret = ""
	}
	rows.Close()
	if err != nil {
		return snapshot, err
	}
	if rows.Err() != nil {
		return snapshot, rows.Err()
	}
	if err = copyConfig.Validate(); err != nil {
		return snapshot, err
	}
	return snapshot, tx.Commit(ctx)
}
