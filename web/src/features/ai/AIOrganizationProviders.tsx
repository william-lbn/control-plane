import { useEffect, useState } from 'react';
import { api, ApiError } from '../../api';
import { AIProviders } from './AIProviders';
import type { ProviderCapabilities } from './AIProviders';

export function AIOrganizationProviders({ organizationId }: { organizationId: string }) {
  const [capabilities, setCapabilities] = useState<ProviderCapabilities | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let current = true;
    api<ProviderCapabilities>(
      `/api/v1/organizations/${encodeURIComponent(organizationId)}/ai-providers/capabilities`,
    )
      .then((value) => {
        if (current) setCapabilities(value);
      })
      .catch((e) => {
        if (!current) return;
        if (e instanceof ApiError && e.code === 'organization_byok_disabled') return;
        setError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      current = false;
    };
  }, [organizationId]);
  if (error) return <div role="alert">{error}</div>;
  if (!capabilities) return null;
  return (
    <section aria-label="组织自有 AI 供应商">
      <AIProviders
        key={organizationId}
        organizationId={organizationId}
        capabilities={capabilities}
      />
    </section>
  );
}
