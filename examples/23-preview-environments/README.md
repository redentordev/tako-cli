# Preview Environments

One short-lived copy of the app per pull request, deployed from an abstract
`preview` template that extends `production`, drops the worker, and runs on a
dedicated preview server.

```bash
cp .env.example .env          # fill in your values
tako secrets init
tako secrets set DATABASE_PASSWORD=... -e preview   # .tako/secrets.preview

tako deploy -e production                 # the base app
tako deploy --preview pr-42               # preview-pr-42 at https://pr-42.preview.example.com
tako ps --preview pr-42
tako logs --preview pr-42 --service web
tako preview list
tako preview destroy pr-42 --yes
tako preview prune --yes                  # remove expired previews
```

Before the first preview, point a wildcard DNS record
`*.preview.example.com` at the preview server. Every preview gets its own
fresh database volume; production data is never shared or copied.

See [docs/PREVIEW-ENVIRONMENTS.md](../../docs/PREVIEW-ENVIRONMENTS.md) and,
for pull-request automation, [docs/CI-CD.md](../../docs/CI-CD.md).
