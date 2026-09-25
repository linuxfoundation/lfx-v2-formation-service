# FGA Contract — Formation Service

This document is the authoritative reference for messages the formation service sends to
[lfx-v2-fga-sync](https://github.com/linuxfoundation/lfx-v2-fga-sync).

**Update this document in the same PR as any change to FGA message construction.**

---

## Project Application

The service writes access only for `project_application`. Checklist authorization is inherited
from `project` and is owned by project-service.

### Message Format

Messages use the generic FGA envelope with `object_type`, `operation`, and `data`.

| Subject | Operation | Trigger |
| --- | --- | --- |
| `lfx.fga-sync.update_access` | `update_access` | Create, revise, withdraw, accept, deny |
| `lfx.fga-sync.delete_access` | `delete_access` | Delete |

Both subjects use core NATS publish. A nil publish result means the client accepted the message;
it is not a broker acknowledgement or confirmation that OpenFGA changed.

### update_access

| Field | Value |
| --- | --- |
| `object_type` | `project_application` |
| `operation` | `update_access` |
| `data.uid` | Application UID |
| `data.public` | `false` |

#### Relations

| Relation | Value |
| --- | --- |
| `submitter` | Stored submitter username |

#### References

| Relation | Value |
| --- | --- |
| `formation_team` | `team:{APPLICATION_FORMATION_TEAM}#member` |

The complete grant set is sent on every update. No `exclude_relations` are used. An empty
application UID, submitter username, or formation-team name is rejected before publishing.

The access message is published before the index message so a successfully indexed document does
not precede its access tuples.

### delete_access

Delete carries only the application UID:

| Field | Value |
| --- | --- |
| `object_type` | `project_application` |
| `operation` | `delete_access` |
| `data.uid` | Application UID |

fga-sync removes publisher-managed user tuples, including `submitter`, but preserves team-subject
tuples on relations not prefixed `global_`. The application's non-global `formation_team` tuple
therefore remains after delete.

### Failure Behavior

Immediate serialization or NATS publish failures are logged and do not change the API response.
Each reconcile sweep republishes the complete grant set for every live application and retries
`delete_access` for every retained deletion marker. Database writes commit before publication.
Timed-out or reordered core NATS delivery is repaired by the retained source state; strict
stale-event rejection requires revision-aware handling in fga-sync.

Changing `APPLICATION_FORMATION_TEAM` does not revoke the old team's existing tuples because
fga-sync preserves team-subject grants on the non-global `formation_team` relation. Such a change
requires an explicit tuple cleanup.
