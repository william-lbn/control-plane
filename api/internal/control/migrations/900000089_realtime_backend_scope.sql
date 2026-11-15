CREATE OR REPLACE FUNCTION valid_backend_scopes(value text[]) RETURNS boolean
 LANGUAGE SQL IMMUTABLE AS $$
 SELECT cardinality(value) BETWEEN 1 AND 4
 AND value <@ ARRAY['ai_gateway:invoke','storage:read','storage:write','realtime:subscribe']::text[]
 AND array_position(value,NULL) IS NULL
 AND cardinality(value)=(SELECT count(DISTINCT item) FROM unnest(value) item)
$$;
