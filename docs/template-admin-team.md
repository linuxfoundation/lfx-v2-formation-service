# Template Admin Team

The template admin API routes (`GET/POST /templates`, `PUT/POST /templates/{uid}`,
`POST /templates/{uid}/publish`, `POST /templates/{uid}/archive`) are guarded at the
gateway by membership of a global OpenFGA team.

## Team name

```
global_formation_template_admin
```

This value is set in `charts/lfx-v2-formation-service/values.yaml` under
`app.templateAdminTeamName` and can be overridden per environment.

## Creating the team

The team object must exist in OpenFGA before any template admin route can be
reached. Use the platform's `fga` CLI or the admin tooling to create it:

```bash
fga tuple write \
  --store-id <store-id> \
  '{"user":"team:global_formation_template_admin#member","relation":"member","object":"team:global_formation_template_admin"}'
```

Or via the FGA REST API — consult the platform FGA documentation for the
authoritative procedure.

## Adding a member

To grant a user template-admin access, write a `member` tuple for
`team:global_formation_template_admin`:

```bash
fga tuple write \
  --store-id <store-id> \
  '{"user":"user:<username>","relation":"member","object":"team:global_formation_template_admin"}'
```

Replace `<username>` with the user's platform username (the subject the gateway
resolves from their OIDC token).

## Removing a member

```bash
fga tuple delete \
  --store-id <store-id> \
  '{"user":"user:<username>","relation":"member","object":"team:global_formation_template_admin"}'
```

## Notes

- Membership changes take effect immediately — no service restart is required.
- The team carries no relation to any project; the guard is purely about
  platform-wide template-admin standing.
- The `formation` team (which gates checklist item status changes) is separate
  from this team and its members do not automatically receive template-admin
  access.
