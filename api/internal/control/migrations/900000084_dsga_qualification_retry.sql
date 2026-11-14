-- Keep each Job, its terminal identity and its qualified successor append-only.
ALTER TABLE dsga_creation_supply_facts DROP CONSTRAINT dsga_creation_supply_facts_kind_check;
ALTER TABLE dsga_creation_supply_facts ADD CONSTRAINT dsga_creation_supply_facts_kind_check
 CHECK (kind IN ('collector_config','qualification_job','qualification_result','runtime_selection')
 OR kind ~ '^(qualification_retry|qualification_job_retry)_[1-9][0-9]{0,9}$');
