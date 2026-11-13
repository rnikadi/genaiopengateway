-- Organization names are unique without case (ADR 0029): role mappings
-- find organizations by name. Existing duplicates, except the oldest,
-- get their id appended so the index can be built.
UPDATE orgs SET name = name || ' (' || id || ')'
WHERE EXISTS (
    SELECT 1 FROM orgs o2
    WHERE lower(o2.name) = lower(orgs.name)
      AND (o2.created_at < orgs.created_at OR (o2.created_at = orgs.created_at AND o2.id < orgs.id))
);
CREATE UNIQUE INDEX orgs_name ON orgs (lower(name));
