-- Run as the metadata owner with psql -v gateway_role=neon_storage_gateway.
-- Provision LOGIN/password separately; this script contains no credentials.
-- The role must be fresh, NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS,
-- have no parent role memberships, own no application objects and receive no
-- default table privileges. Do not reuse the API/Worker database identity.
-- Startup also rejects CREATE on the active metadata schema and any parent-role
-- membership, including NOINHERIT membership that could permit SET ROLE.
-- Each listed table/column grant and each row-lock EXECUTE is required: granting
-- only SELECT without INSERT must not pass startup/readiness.
GRANT USAGE ON SCHEMA public TO :"gateway_role";
GRANT SELECT ON schema_migrations,projects,branches,endpoints,object_storage_instances,
 organizations,organization_members,project_grants,backend_credentials,backend_storage_signing TO :"gateway_role";
GRANT SELECT(id,disabled) ON users TO :"gateway_role";
GRANT UPDATE(last_used_at) ON backend_credentials TO :"gateway_role";
GRANT EXECUTE ON FUNCTION storage_gateway_lock_project(text,text),storage_gateway_lock_branch(text,text),storage_gateway_lock_instance(text,text) TO :"gateway_role";
GRANT SELECT ON storage_gateway_limits TO :"gateway_role";
GRANT SELECT,INSERT ON storage_gateway_days,storage_gateway_requests TO :"gateway_role";
GRANT UPDATE(requests,observed_bytes,reserved_bytes) ON storage_gateway_days TO :"gateway_role";
GRANT UPDATE(state,finished_at,http_status,ingress_bytes,egress_bytes,duration_ms) ON storage_gateway_requests TO :"gateway_role";
GRANT SELECT,INSERT ON storage_gc_allocations TO :"gateway_role";
GRANT UPDATE(state,resolved_at) ON storage_gc_allocations TO :"gateway_role";
-- Storage publishes the SAME transactional object-event outbox as API writes.
-- This grants no Functions deployment/invocation/Guest CA permission.
GRANT SELECT ON function_triggers,function_definitions,storage_event_branches TO :"gateway_role";
GRANT INSERT ON storage_event_branches TO :"gateway_role";
GRANT UPDATE(endpoint_id,database_name,timeline_id,generation,dirty,error_code,updated_at) ON storage_event_branches TO :"gateway_role";
-- Existing native presigned-upload completion appends its audit event.
GRANT INSERT(actor_id,action,resource_type,resource_id,outcome,request_id) ON audit_events TO :"gateway_role";
