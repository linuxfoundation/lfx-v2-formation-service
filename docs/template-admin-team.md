# Template Admin Team

The template admin API routes (`GET /formations/templates`, `POST /formations/templates`,
`GET /formations/templates/{uid}`, `PUT /formations/templates/{uid}`,
`POST /formations/templates/{uid}/publish`, `POST /formations/templates/{uid}/archive`)
are guarded at the gateway by membership of a global OpenFGA team.

## Team name

```
global_formation_template_admin
```

This value is set in `charts/lfx-v2-formation-service/values.yaml` under
`app.templateAdminTeamName` and can be overridden per environment.

## Creating the team

The `team:global_formation_template_admin` object is provisioned through the
platform's admin tooling (not a raw FGA tuple). Contact the platform team or
follow the internal team-provisioning runbook to create it. Once the team
object exists, members are added with the tuples below.

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
