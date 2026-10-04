import { useEffect, useState } from 'react';
import type { Endpoint } from '../api';

// Keep the same database target across SQL, monitoring, Compute and connection
// pages in this tab. Only an Endpoint ID is stored; credentials stay in memory.
export function useEndpointSelection(projectId: string, endpoints: Endpoint[]) {
  const key = `neon-console.endpoint:${projectId}`;
  const preferred = () =>
    endpoints.find((endpoint) => endpoint.observed_state === 'active')?.id ||
    endpoints[0]?.id ||
    '';
  const [selected, setSelection] = useState(() => {
    try {
      return sessionStorage.getItem(key) || preferred();
    } catch {
      return preferred();
    }
  });
  function setSelected(id: string) {
    setSelection(id);
    try {
      sessionStorage.setItem(key, id);
    } catch {
      // Selection still works when browser storage is unavailable.
    }
  }
  useEffect(() => {
    if (endpoints.length && !endpoints.some((endpoint) => endpoint.id === selected)) {
      setSelected(preferred());
    }
  }, [selected, endpoints]);
  return [selected, setSelected] as const;
}
