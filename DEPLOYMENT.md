# Deployment

The normal release path is `.github/workflows/image.yml`: push to `master`, wait
for **Publish bot image** to succeed, then deploy its matching immutable images:

- `ghcr.io/akorzunin/arr-telegram-bot:COMMIT_SHA`
- `ghcr.io/akorzunin/arr-telegram-bot:worker-COMMIT_SHA`

The workflow authenticates to GHCR using Actions' own `GITHUB_TOKEN`. Local `gh`
CLI authentication is independent of Git push and workflow publishing credentials.

Update both image pins in `../ansible-playbooks/playbooks/arr/compose.yaml` and the
workstation's `/srv/deploy/arr/compose.yaml`. Recreate only the affected services:

```sh
cd /srv/deploy/arr
docker compose --profile bot --profile prepare pull bot prepare
docker compose --profile bot --profile prepare up -d --no-deps bot prepare
```

Check for `compose.override.yaml` before deploying. A temporary local-build override
was used for the item-ID/menu/magnet fixes; archive that override when switching to
published images, otherwise Compose will keep selecting the local images. Rollback
requires restoring the previous image pins and recreating these same two services.

## GPU recovery

The Gentlemen's preparation failed because the old container returned
`CUDA_ERROR_NO_DEVICE`, while the host GPU was healthy. Recreating only the worker
restored GPU access. A synthetic encode and a short encode of the actual source
passed; the failed job was backed up and requeued without changing original media.

The temporary source snapshot and pre-deployment backups remain under
`/srv/deploy/arr/build-fixes-12cebc915b6d` and
`/srv/deploy/arr/backups/fixes-12cebc915b6d` respectively.
Do not restore notification/offset snapshots casually: doing so can replay updates
or lose tracking for requests made since deployment.
