-- Forward-only catalog transition. Owner changes keep the original database,
-- data and credential references; no role is renamed or granted extra rights.
ALTER TABLE branch_databases DROP CONSTRAINT branch_databases_state_check;
ALTER TABLE branch_databases ADD CONSTRAINT branch_databases_state_check
 CHECK (state IN ('creating','ready','updating','deleting','deleted'));
