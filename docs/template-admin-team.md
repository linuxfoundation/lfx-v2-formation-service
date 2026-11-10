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

## The team exists once it has a member

There is no separate step that creates `team:global_formation_template_admin`.
An OpenFGA object exists as soon as a tuple names it, so the team comes into
being with its first `member` tuple, and until then every template admin route
is refused. The team is not listed in sso-tools today, so members are managed
through lfx-v2-fga-sync rather than through that UI.

## Adding a member

Publish a `member_put` message to lfx-v2-fga-sync. It is an asynchronous
JetStream message and fga-sync sends no reply:

```bash
nats pub lfx.fga-sync.member_put \
  '{"object_type":"team","operation":"member_put","data":{"uid":"global_formation_template_admin","username":"<username>","relations":["member"]}}'
```

Replace `<username>` with the user's platform username, without the `user:`
prefix (the subject the gateway resolves from their OIDC token). Record who
added whom: the OpenFGA change log shows when a tuple was written, not by whom.

## Removing a member

```bash
nats pub lfx.fga-sync.member_remove \
  '{"object_type":"team","operation":"member_remove","data":{"uid":"global_formation_template_admin","username":"<username>","relations":["member"]}}'
```

## Notes

- Membership changes take effect once fga-sync has processed the message — no
  service restart is required.
- The team carries no relation to any project; the guard is purely about
  platform-wide template-admin standing.
- The `formation` team (which gates checklist item status changes) is separate
  from this team and its members do not automatically receive template-admin
  access.
