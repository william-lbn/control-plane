import { AsyncLocalStorage } from "node:async_hooks";
import type { Pool, PoolClient } from "pg";

type Scope = {
  client?: PoolClient;
  savepoints: string[];
  nextSavepoint: number;
  rejection?: string;
  closed?: boolean;
  mailTasks: Promise<boolean>[];
};
const rejections: Record<string, { status: number; code: string; message: string }> = {
  P4001: { status: 403, code: "organization_limit_reached", message: "Organization membership limit reached" },
  P4002: { status: 403, code: "organization_member_limit_reached", message: "Organization member limit reached" },
  P4003: { status: 409, code: "organization_owner_required", message: "Transfer organization ownership before removing its last owner" },
  P4004: { status: 422, code: "invalid_organization_role", message: "Unsupported organization role" },
  P4005: { status: 404, code: "organization_disabled", message: "Organization management is disabled" },
  P4006: { status: 409, code: "organization_manager_required", message: "Assign another organization manager before removing its last manager" },
};

// The maintained plugin has multi-query mutations and nested transactions.
// Buffer the HTTP outcome until one outer transaction commits, preserving its
// own nested transactions as savepoints. Rate-limit writes remain autocommit:
// Better Auth 1.7.7 consumes them before endpoint/session database work.
export function transactionalAuthDatabase(pool: Pool) {
  const scopes = new AsyncLocalStorage<Scope>();
  const database = new Proxy(pool, {
    get(target, property) {
      if (property === "query") return async (...args: any[]) => {
        if (!scopes.getStore() || scopes.getStore()?.closed) return (target.query as any)(...args);
        const client = await database.connect();
        try { return await (client.query as any)(...args); }
        finally { client.release(); }
      };
      if (property !== "connect") {
        const value = Reflect.get(target, property, target);
        return typeof value === "function" ? value.bind(target) : value;
      }
      return async () => {
        const scope = scopes.getStore();
        if (!scope || scope.closed) return target.connect();
        let client = scope.client || await target.connect();
        let released = false;
        return new Proxy(client, {
          get(connection, key) {
            if (key === "release") return () => {
              if (scope.client !== connection && !released) {
                released = true;
                connection.release();
              }
            };
            if (key !== "query") {
              const value = Reflect.get(connection, key, connection);
              return typeof value === "function" ? value.bind(connection) : value;
            }
            return async (...args: any[]) => {
              if (scope.closed) {
                const independent = await target.connect();
                try { return await (independent.query as any)(...args); }
                finally { independent.release(); }
              }
              const sql = typeof args[0] === "string" ? args[0] : args[0]?.text;
              if (typeof sql !== "string" || typeof args.at(-1) === "function")
                throw new Error("A maintained Promise SQL query is required");
              if (sql.includes('"rateLimit"')) {
                if (scope.client) throw new Error("Rate limiting must precede transactional endpoint work");
                return (connection.query as any)(...args);
              }
              if (!scope.client) {
                scope.client = connection;
                await connection.query("BEGIN");
              } else if (scope.client !== connection && !released) {
                released = true;
                connection.release();
              }
              client = scope.client;
              const command = sql.trim().replace(/;$/, "").toLowerCase();
              try {
                if (command === "begin" || command === "begin transaction" || command.startsWith("start transaction")) {
                  const name = "neon_auth_request_" + scope.nextSavepoint++;
                  scope.savepoints.push(name);
                  return client.query("SAVEPOINT " + name);
                }
                if (command === "commit" || command === "rollback") {
                  const name = scope.savepoints.pop();
                  if (!name) throw new Error("Unexpected maintained transaction boundary");
                  if (command === "rollback") await client.query("ROLLBACK TO SAVEPOINT " + name);
                  return client.query("RELEASE SAVEPOINT " + name);
                }
                return await (client.query as any)(...args);
              } catch (error) {
                const code = (error as {code?:string})?.code;
                if (code && Object.hasOwn(rejections, code)) {
                  scope.rejection = code;
                  // Kysely may log database errors independently of the Auth
                  // logger. Expected guards must not expose SQL/row details.
                  throw Object.assign(new Error(rejections[code].message),{code});
                }
                throw error;
              }
            };
          },
        });
      };
    },
  });
  async function handle(request: Request, handler: (request: Request) => Promise<Response>) {
    const action = new URL(request.url).pathname;
    if (request.method !== "POST" || (!action.includes("/organization/") && !action.endsWith("/admin/remove-user")))
      return handler(request);
    const scope: Scope = { savepoints: [], nextSavepoint: 0, mailTasks: [] };
    return scopes.run(scope, async () => {
      let connectionError: Error | undefined;
      try {
        const response = await handler(request);
        const mailFailed=scope.mailTasks.length>0&&!(await Promise.all(scope.mailTasks)).every(Boolean);
        if (scope.client) await scope.client.query(response.status >= 400 || scope.rejection || mailFailed ? "ROLLBACK" : "COMMIT");
        if(mailFailed)return Response.json({code:"organization_mail_unavailable",message:"Invitation delivery could not be confirmed"},{status:503,headers:{"Cache-Control":"no-store"}});
        if (scope.rejection) {
          const rejection = rejections[scope.rejection];
          return Response.json({code:rejection.code,message:rejection.message},{status:rejection.status,headers:{"Cache-Control":"no-store"}});
        }
        return response;
      } catch {
        if (scope.client) try { await scope.client.query("ROLLBACK"); }
        catch { connectionError = new Error("Auth transaction connection unavailable"); }
        return Response.json({code:"auth_operation_unavailable",message:"Application operation could not be confirmed"},{status:503,headers:{"Cache-Control":"no-store"}});
      } finally {
        scope.closed = true;
        scope.client?.release(connectionError);
      }
    });
  }
  function trackOrganizationMail(task:Promise<void>){
    const scope=scopes.getStore();
    if(scope&&!scope.closed)scope.mailTasks.push(task.then(()=>true,()=>false));
    return task;
  }
  return { database, handle, trackOrganizationMail };
}
