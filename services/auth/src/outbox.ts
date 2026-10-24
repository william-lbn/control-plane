import { randomUUID } from "node:crypto";
import type { Pool } from "pg";
import type { createAuthWebhooks, AuthEventEnvelope } from "./webhooks.ts";

const nonblocking = new Set(["user.created", "phone_number.verified", "organization.invitation.created", "organization.invitation.accepted"]);
export function createAuthOutbox(database: Pool, branch: string, hooks: ReturnType<typeof createAuthWebhooks>, events: string[]) {
  const enabled = events.some(type => nonblocking.has(type));
  let active: Promise<{ delivered: number; failed: number }> | undefined;
  let requested=0,scheduled:Promise<void>|undefined;
  function schedule():Promise<void>{
    // Coalesce activity without losing requests arriving during an in-flight
    // delivery. Each signal funds one bounded batch; never poll an idle DB.
    if(!enabled)return Promise.resolve();requested=Math.min(1000,requested+2);
    if(scheduled)return scheduled;
    scheduled=(async()=>{while(requested>0){requested=Math.max(0,requested-2);const result=await drain(2);if(result.delivered+result.failed===0){requested=0;break}}})();
    return scheduled.finally(()=>{scheduled=undefined});
  }
  async function drain(limit = 2): Promise<{ delivered: number; failed: number }> {
    if (!enabled) return { delivered: 0, failed: 0 };
    if (!Number.isInteger(limit) || limit < 1 || limit > 3) throw new Error("Bounded Auth event batch required");
    if (active) return active;
    active = (async () => {
      let delivered = 0, failed = 0;
      await database.query(`UPDATE control_webhook_event e SET state='failed',lease_token=NULL,lease_until=NULL,last_error='delivery_lease_exhausted'
        FROM control_webhook_policy p WHERE p.singleton AND p.enabled AND p.branch_id=$1 AND e.branch_id=$1
        AND e.generation=p.generation AND e.state='delivering' AND e.lease_until<now() AND e.delivery_batches>=3`, [branch]);
      for (let index = 0; index < limit; index++) {
        const token = randomUUID();
        const claimed = await database.query(`WITH candidate AS (
          SELECT e.id FROM control_webhook_event e JOIN control_webhook_policy p ON p.singleton
          WHERE p.enabled AND p.branch_id=$1 AND e.branch_id=$1 AND e.generation=p.generation AND e.delivery_batches<3
          AND (e.state='pending' OR (e.state='delivering' AND e.lease_until<now()))
          ORDER BY e.created_at,e.id FOR UPDATE OF e SKIP LOCKED LIMIT 1
        ) UPDATE control_webhook_event e SET state='delivering',delivery_batches=delivery_batches+1,
          lease_token=$2,lease_until=now()+interval '20 seconds'
          FROM candidate c WHERE e.id=c.id RETURNING e.id,e.payload`, [branch, token]);
        const row = claimed.rows[0]; if (!row) break;
        let state = "delivered";
        try { await hooks.deliver(row.payload as AuthEventEnvelope); delivered++; }
        catch { state = "failed"; failed++; }
        await database.query(`UPDATE control_webhook_event SET state=$3,lease_token=NULL,lease_until=NULL,
          last_error=CASE WHEN $3='failed' THEN 'delivery_unavailable' ELSE NULL END,
          delivered_at=CASE WHEN $3='delivered' THEN now() ELSE NULL END WHERE id=$1 AND lease_token=$2 AND state='delivering'`, [row.id, token, state]);
      }
      return { delivered, failed };
    })();
    try { return await active; } finally { active = undefined; }
  }
  async function list() {
    if (!enabled) return { items: [], dropped_events: 0 };
    const result = await database.query(`SELECT id,event_type,state,delivery_batches,last_error,created_at,delivered_at
      FROM control_webhook_event WHERE branch_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, [branch]);
    const policy = await database.query('SELECT dropped_events FROM control_webhook_policy WHERE singleton AND branch_id=$1', [branch]);
    return { items: result.rows, dropped_events: Number(policy.rows[0]?.dropped_events || 0) };
  }
  return { drain, list, schedule, enabled };
}
