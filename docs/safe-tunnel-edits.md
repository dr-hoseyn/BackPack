# Reviewing and undoing tunnel edits

The terminal Edit menu and the web panel show the current and proposed values
before applying changes. Credentials appear as **Hidden value**; the preview
never includes a raw configuration file. Cancel, Enter at the confirmation, or
closed terminal input leaves the settings untouched.

In the panel, **Save** opens the review. **Apply and restart** commits the
reviewed settings. Forwarded ports have separate source and target fields, an
Add mapping button and a Delete button per row. Removing the last row is
explicit, and the review shows the empty list. Direct layer-4 edges still
require at least one mapping.

If another editor changes the config after the review, applying is refused.
Reopen the form and review the latest settings before trying again.

## Applying changes

All tunnel settings writers use one transaction: parse the proposed file,
write it atomically, restart the local service, and verify startup readiness.
Managed carriers also need their helper's readiness response. An unchanged
configuration does not restart the service or add a history entry.

If startup fails, the previous file is restored. A service that was running is
restarted on that file; a service that was stopped is stopped again. A failed
recovery is reported explicitly rather than claiming that rollback succeeded.
Direct/L3 edits preserve carrier settings and auxiliary configuration tables.

Startup readiness is a local check. It does not prove end-to-end traffic or
that the peer uses matching settings. Changes to paired settings still need
the other end to follow; managed-peer update failures are reported separately.

## Undoing a successful change

Open **Undo a change** in the terminal or **History** in the panel editor.
Choose an entry, review its differences from the current config, then apply.
Restoring uses the same transaction and refuses a stale preview too. A
successful restore saves the replaced configuration as another history entry,
so that restore can itself be undone.

History keeps the ten most recent superseded configurations, including their
credentials. Files are written atomically with mode `0600` under
`/etc/backpack/history/` and are included in server backups. Recording history
is best effort: a history-storage failure does not turn an already successful
service change into a failed edit.

## API compatibility

`POST /api/tunnel/preview` accepts the same form as `/api/tunnel/edit` and
returns a `revision` plus a redacted `changes` list. Submit the revision with
the edit to protect the reviewed snapshot. Legacy callers can continue using
the edit endpoint without a revision and use the same rollback path.

`POST /api/confhist/preview` accepts `name` and the entry's string `at`
timestamp. Pass its revision to `/api/confhist/restore`. These preview
endpoints require write authentication, like the settings editor and history.
