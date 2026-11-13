# Encryption keys and KMS

The gateway encrypts what it stores that must stay secret: provider API keys set in the UI, team credentials, users' MCP tokens, webhook and stream URLs, and logged request bodies. It uses one 32-byte key, `secrets.encryption_key`, plus older keys under `secrets.previous_keys` that still decrypt during a rotation.

You can put the key in the config file, usually from an environment variable:

```yaml
secrets:
  encryption_key: ${GATEWAY_ENCRYPTION_KEY}      # openssl rand -base64 32
```

Or you can keep it wrapped by a key management service. The config then holds only ciphertext. At startup, the gateway asks the KMS to unwrap it, and the plaintext key exists only in the gateway's memory. Revoking the gateway's access to the KMS key stops new replicas from starting, and leaves the stored data unreadable without the KMS.

```yaml
secrets:
  kms:
    provider: aws                                   # aws, gcp or vault
    key: arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab
    wrapped_key: AQICAHh…                           # base64 ciphertext of the 32-byte key
    previous_wrapped_keys: []                       # wrapped previous_keys, during a rotation
```

`encryption_key` and `kms` cannot both be set. Each wrapped key is unwrapped once per process, so config reloads do not call the KMS again.

## AWS KMS

The gateway calls `Decrypt` with the AWS default credential chain: environment variables, an IRSA or EKS Pod Identity role, or an instance profile. The region comes from the key ARN. The role needs `kms:Decrypt` on the key.

Wrap a new key:

```sh
openssl rand 32 > key.bin
aws kms encrypt --key-id "$KEY_ARN" --plaintext fileb://key.bin --query CiphertextBlob --output text
shred -u key.bin
```

Set `endpoint` to use a VPC endpoint, such as `https://vpce-….kms.eu-west-1.vpce.amazonaws.com/`.

## Google Cloud KMS

The gateway calls `decrypt` with Application Default Credentials, such as Workload Identity on GKE. The service account needs `roles/cloudkms.cryptoKeyDecrypter` on the key. `key` is the key's resource name, `projects/P/locations/L/keyRings/R/cryptoKeys/K`.

```sh
openssl rand 32 > key.bin
gcloud kms encrypt --key K --keyring R --location L --plaintext-file key.bin --ciphertext-file - | base64 -w0
shred -u key.bin
```

## HashiCorp Vault Transit

The gateway calls `POST /v1/<mount>/decrypt/<key>` on the Vault at `endpoint`, or at `VAULT_ADDR` when no endpoint is set. `mount` defaults to `transit`, and `VAULT_NAMESPACE` is honored.

- **Token**: the gateway uses `VAULT_TOKEN`.
- **Kubernetes**: set `role`, and the gateway logs in with its pod's service account token through `auth/<auth_mount>/login`. `auth_mount` defaults to `kubernetes`. The role's policy needs `update` on `<mount>/decrypt/<key>`.

```sh
openssl rand -base64 32 | vault write -field=ciphertext transit/encrypt/gateway plaintext=-
```

`wrapped_key` is then the `vault:v1:…` string.

## Rotating the key

1. Generate and wrap a new key.
2. Move the current `wrapped_key` into `previous_wrapped_keys`, put the new one in `wrapped_key`, and roll the replicas. New writes use the new key, and old data still decrypts.
3. Keep each old key while data encrypted with it is stored. Users' MCP tokens are re-encrypted with the current key on their next refresh. Stored objects are re-encrypted when they are next saved. Logged bodies keep their key until the body logging retention removes them.

## Provider keys and other secrets from Vault

Secrets in the config file, such as provider API keys, are read from environment variables (`${OPENAI_API_KEY}`). To source them from Vault or a cloud secret manager, inject them into the environment with Vault Agent, the Vault Secrets Operator, or the External Secrets Operator. The Helm chart's `envFrom` takes the resulting Kubernetes Secret.
