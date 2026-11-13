# Backup, restore and upgrades

The gateway keeps its state in one database: organizations, teams and keys, stored configuration, Cedar policies, MCP pins and connections, usage, the audit log and logged bodies. This page covers backing that database up, restoring it, moving between SQLite and Postgres, and upgrading safely.

## Backing up

```
genaiopengateway backup -config config.yaml -out gateway-2026-11-12.gogb
genaiopengateway backup -data /var/lib/genaiopengateway -out backup.gogb   # a gateway started without a config file
```

The command reads the database the config names, in one read-only transaction, so the archive is a consistent snapshot taken while the gateway keeps serving. It is written beside the target name and renamed into place, so a failed backup never leaves a partial file. `-out -` writes to standard output, for piping to object storage:

```
genaiopengateway backup -config config.yaml -out - | aws s3 cp - s3://backups/gateway/$(date +%F).gogb
```

The archive is gzip-compressed JSON lines: a header with the format, the schema version, the source driver, the time and each table's row count, then one line per row. It holds every table except:

- `admin_sessions`: a restored gateway does not accept the old gateway's sign-in cookies, so people sign in again;
- `schema_migrations` and `config_revision`, which the target keeps for itself.

**Secrets stay encrypted.** Provider keys, team credentials, users' MCP tokens and logged bodies are in the archive as the gateway stores them, encrypted with `secrets.encryption_key` ([secrets.md](secrets.md)). The key is never in the archive, so keep it, or the KMS or Vault reference that holds it, with your backups. The header carries a short fingerprint of the key so that a restore can tell whether the target has the right one; the key cannot be derived from it. Key hashes are in the archive; key secrets are not, since the gateway never stores them.

The archive is still sensitive: it holds email addresses, usage and the audit log. Store it encrypted and limit who can read it. The file is created readable only by its owner.

### Postgres

On Postgres, your database's own tools remain a good choice, and are faster for large databases: `pg_dump`, a managed service's snapshots, or point-in-time recovery. The gateway's archive adds a format that moves between drivers and versions, and a restore that checks the schema and the key. Many teams run both: database snapshots for disaster recovery, and a nightly `genaiopengateway backup` for moves and audits.

### On Kubernetes

Run the backup as a CronJob with the gateway image and the same Secret and config the Deployment mounts:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata: {name: gateway-backup}
spec:
  schedule: "0 2 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: backup
              image: ghcr.io/rnikadi/genaiopengateway:1.1.0   # the version you run
              args: [backup, -out, /backup/gateway.gogb]   # the entrypoint already passes -config
              envFrom: [{secretRef: {name: gateway-env}}]
              volumeMounts:
                - {name: config, mountPath: /etc/genaiopengateway}
                - {name: backup, mountPath: /backup}
          volumes:
            - {name: config, configMap: {name: gateway-config}}
            - {name: backup, persistentVolumeClaim: {claimName: gateway-backup}}
```

## Restoring

```
genaiopengateway restore -check backup.gogb                     # what the archive holds, without restoring
genaiopengateway restore -config config.yaml backup.gogb        # into an empty database
genaiopengateway restore -config config.yaml -replace backup.gogb   # delete what is there first
```

A restore runs in one transaction: if anything fails, the database is left as it was. It refuses:

- a database that already has data, unless `-replace` is given;
- an archive from a newer gateway than the binary (restore it with that version or later); an archive from an older gateway is fine;
- an archive whose secrets need a different encryption key from the one the config has as `secrets.encryption_key` or in `previous_keys`, unless `-ignore-key` is given. Without the key, stored provider keys and credentials cannot be read after the restore and must be entered again;
- a truncated archive: each table's row count must match the header.

The restore can run while gateways are serving from the database. They pick up the restored configuration, organizations and keys within `store.refresh_interval`; usage counters held in Valkey are not part of the database and carry on.

### Moving from SQLite to Postgres

A gateway started with zero config, or a small install, runs on SQLite. To move it to Postgres:

1. Write a config for Postgres with the same `secrets.encryption_key`. For a zero-config start, the key is `encryption_key` in `bootstrap.json` in the data directory.
2. `genaiopengateway backup -data /var/lib/genaiopengateway -out move.gogb` (or `-config` with the SQLite config).
3. `genaiopengateway restore -config postgres.yaml move.gogb`. The gateway creates the tables in the empty Postgres database first.
4. Start the gateway with the Postgres config.

Moving back from Postgres to SQLite works the same way.

## Upgrading

Migrations run at startup and only add tables, columns with defaults and indexes, so a database is upgraded by starting the new version on it; on Postgres, one replica migrates while the others wait. Before upgrading:

1. Take a backup with the version you run now.
2. Read the release's upgrade notes in the [changelog](https://github.com/rnikadi/genaiopengateway/blob/main/CHANGELOG.md).
3. Roll the new version out. On Kubernetes, the chart's rolling update replaces one replica at a time; old and new replicas can serve together during the rollout within a minor version series.

**Going back** to an older version is done by restoring the backup taken before the upgrade with the older binary: an older binary refuses a database archive from a newer schema, and old binaries started on a migrated database ignore the new tables and columns but are not tested that way. Usage and audit entries written since the backup are lost, so export them first if you need them (`GET /admin/v1/usage`, `GET /admin/v1/audit`).

### How this is tested

CI runs [`scripts/upgrade-test.sh`](https://github.com/rnikadi/genaiopengateway/blob/main/scripts/upgrade-test.sh) on every change. It builds the previous release from its tag, gives it an organization, a team, a key and a stored provider and model, then starts the new build on the same database and checks that all of them still work, the old key included. It then backs the database up, restores it into an empty one, starts there and checks again. The store's tests restore a SQLite archive into Postgres and back, and compare every row.
