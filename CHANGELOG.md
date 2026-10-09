# Changelog

All notable changes to Quetzal are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
releases may include breaking changes).

## [Unreleased]

### Fixed

- **Newly granted creation rights take effect without signing in again.**
  The denied-creation notice refreshes the account on entry and offers Check
  access again, so an administrator granting a quota does not leave an open
  session permanently stuck behind its previous zero quota.
- **A language switch also updates errors on retained server tabs.** File,
  schedule and server-list/detail loaders keep the original error and translate
  it when displayed, so a later refresh cannot reuse the previous language.
  Changing language does not restart polling or discard metric history.
- **The documentation home page keeps its theme picker on phones.** Light,
  Dark and Auto remain reachable without entering a guide. The compact header
  fits at 320 px while preserving the logo's 140 px brand minimum.
- **Documentation search reports failures and can retry.** Search keeps the
  query, presents an error instead of an endless loading message, and rebuilds
  the failed Pagefind instance on Retry. Results, sub-results, pagination and
  empty searches use the real index; the modal preserves focus and locks
  background scrolling. Verification distinguished an audit-browser worker
  instrumentation failure from ordinary Chromium, where the index already
  worked; no speculative Pagefind upgrade or worker patch is applied.
- **The documentation's light-theme selection is readable.** Active navigation
  and accent text use the existing darker rust token (6.08:1 white-on-rust),
  without recolouring the brand artwork.
- **Role help links to the Users tab.** It no longer points to a nonexistent
  Users card above the role editor.
- **The mobile sign-in logo keeps its proportions.** Only the horizontal
  top-bar lockup is resized on phones, at the brand's 140 px minimum width;
  the stacked authentication logo remains 168 px wide at its natural ratio.
- **Scheduled actions use translated names.** Both the action picker and saved
  task chains say Start, Stop, Restart, Command and Backup in the selected
  language, while the API values stay unchanged.
- **File sorting works from the keyboard.** Name, Size and Modified are native
  buttons with the panel's focus ring; their headers expose the active sort
  direction instead of relying on a visual arrow and a mouse-only click.
- **A zero server quota is explained before filling a form.** The server list
  disables creation with guidance to ask an administrator, and a direct create
  URL shows that guidance instead of a form that must fail. Administrators
  with server-management permission retain the API's quota exemption.
- **Form validation follows the selected language.** API errors are formatted
  consistently across the panel, including account/password/link errors,
  permissions, quotas and parameterized resource or schedule validation.
  French messages retain the rejected values; unrecognized technical details
  remain available rather than being replaced by a generic error. New-password
  fields explain and enforce their minimum length before submission.
- **Input examples use the brand's readable muted text colour.** Placeholders
  now have a 6.19:1 contrast on the panel's input surface instead of inheriting
  a browser grey below the normal-text accessibility threshold.
- **Every administration tab stays visible.** Tab bars wrap at desktop and
  tablet widths as well as on phones, so Notifications and Activity no longer
  hide beyond a scrollbar that was not displayed.
- **Template pickers close when focus leaves them.** Tab restores the selected
  model instead of leaving an apparently empty field and open menu. The picker
  exposes its list, selection and active option to assistive technology, keeps
  keyboard navigation in range, and never submits the form on an empty search.
- **Authentication cards fit small screens.** Sign-in, setup, recovery and
  account-link pages keep their side margins at 320 px instead of clipping a
  fixed-width card; long addresses and setup commands wrap inside the card.
- **Form labels identify their controls.** Authentication, account, server and
  administration forms associate labels and help text with unique field IDs;
  dynamically repeated fields and groups have distinct accessible names too.
  Clicking a label focuses its control, and screen readers no longer have to
  infer a field's purpose from its placeholder.
- **One API-key creation at a time.** The name and submit button stay locked
  while a key is being created, with a visible progress label; repeated clicks
  cannot create extra keys or replace the token the user has yet to copy.
- **Server tabs keep unfinished schedules and file edits.** Visiting another
  tab no longer resets those drafts; leaving the server, signing out or closing
  the page warns before discarding them. The file editor also asks before
  closing or opening a different file, keeps edits after a failed save, and
  no longer closes a similarly named file when its neighbour is deleted.
- **Editing the idle timeout no longer saves every keystroke.** The hibernation
  delay keeps a local draft, accepts only positive whole minutes, and saves on
  Enter or Save. Pending writes disable the policy controls, and older polls
  cannot overwrite a policy that was just saved.
- **A failed security-settings load is not a policy.** The two-factor and
  invitation cards show loading or an error with Retry until their real values
  arrive, instead of presenting permissive defaults after a failed request.
- **Open a server without a mouse.** Server names in the list are now real
  links: Tab reaches them, Enter opens them, and they can be opened in a new tab.
- **An install that keeps failing says so, instead of reporting "installing"
  for as long as it fails.** A failed install step is retried in place by
  Kubernetes, and the panel read that as a failure only once the step reached
  CrashLoopBackOff. A step that takes minutes and fails at the end never gets
  there: each attempt runs long enough to reset the back-off, so the container
  is simply running again. A modpack install that spent four minutes in `apt`
  before failing therefore looped every five minutes while the server said
  "running install" — and the install log, which shows the current attempt,
  almost always showed the slow part rather than the error at the end of the
  previous one. The phase is now the error, with the step, its exit code, what
  it wrote, and which attempt is running.
- **A clean reinstall no longer deletes `.quetzalignore`.** It is Quetzal's own
  file, not something a modpack shipped, and nobody thinks to add it to the
  paths they keep. Deleting it silently put back into the server's backups
  everything the list had been leaving out — a Steam game's own gigabytes —
  and nothing said so until a restore came up short. It is spared whatever the
  list says. A wipe with no list still takes it: that resets the server on
  purpose.

## [0.15.0] - 2026-10-07

An external security audit of the panel, by @Loulouw, with a regression test for
each of its findings. The file manager is the substantial change: the shell
scripts that carried out file operations are replaced by a helper that works
through root-confined file descriptors, so a symlink planted after a path is
checked no longer wins the race. Alongside it, two things that updating a
modpack and backing up a Steam game had been missing: a reinstall that deletes
everything except the paths you keep, and a `.quetzalignore` that leaves chosen
paths out of a server's backups. The one-step import from a Pterodactyl panel is
gone.

**Upgrading from 0.14.0** — eight things behave differently:

- **Rotate any credential `qctl create` set from a variable its template marks
  secret.** Those values were written to the server's public environment, so
  they reached API readers, database copies and exports. Startup moves them into
  encrypted storage, but the old copies remain. See *Security* and
  `docs/UPGRADE.md`.
- Each server's data-manager pod rolls once: HTTP file operations now use the
  same root-confined helper as SFTP, so they need the helper image even where
  SFTP is off. The game pods are untouched.
- A server with a CPU limit restarts once, to take its new reservation, and a
  managed database host restarts once, for its new probes. See *Changed* and
  *Fixed*.
- A notification channel that sets its own SMTP host may only reach public
  addresses. To send through an internal relay, configure the panel's own email
  settings and let the channel use that relay. See *Security*.
- A stored kubeconfig that names local credential files or runs an
  authentication plugin is refused until it is replaced by a self-contained one.
- A backup, restore or SQL import whose Job disappears is now held for an
  administrator instead of being replayed: its completion is the evidence the
  panel needs before it commits a result. Do not delete those Jobs by hand.
- The one-step import from a Pterodactyl panel is gone. Migrate by creating the
  server from the egg's template, then uploading its files. See *Removed*.
- Building from source needs Go 1.27.

### Added

- **Clean reinstall: delete everything but what you keep, then install.**
  Updating a modpack meant either reinstalling over the old version — its
  install unpacks the new one on top and deletes nothing, so the mods,
  configs and scripts the new version dropped stayed, mods in two versions
  crash the server at start — or deleting by hand, in the file manager, two
  dozen folders the pack had brought. A reinstall now offers a third choice
  next to keeping the files and wiping them: delete all of them except a
  list of paths, then run the install. A Minecraft Java template offers the
  worlds, `server.properties`, the player lists and `plugins`; another
  template's administrator can set its own list (`reinstallKeep`). Before
  anything is deleted, the panel shows which of the server's files will be
  kept and which deleted, and names a path that matches nothing — a world
  that is not called `world`. The server remembers its list for the next
  update, and the API takes it as `keep` on `POST /api/servers/{id}/reinstall`.
  The deletion walks down only into the folders that hold a kept path,
  follows no link, and picks up where it stopped if the install is
  interrupted; servers that have no clean reinstall pending keep exactly the
  pod they had, so upgrading Quetzal restarts none of them.
- **`.quetzalignore`: leave paths out of a server's backups.** A Steam game's
  server is mostly the game itself — gigabytes SteamCMD downloads again on
  demand — around a few megabytes of saves, and every backup copied all of it.
  A `.quetzalignore` at the root of the server's files now lists what its
  backups leave out, one pattern per line, as a `.gitignore` does (`/Engine/`,
  `*.log`, `!keep.log`), with the limits Pterodactyl puts on its `.pteroignore`:
  32 KiB, 256 patterns, 16 wildcards a pattern. Each backup records the list it
  applied (`ignored`), and the panel shows it. Restoring the backup makes the
  other files the backup's and leaves those paths as they are, so the game
  stays in place where a restore that matched the volume to the snapshot would
  have deleted it. A list the backup cannot apply makes it copy every file and
  say why, rather than fail; a transfer to another cluster always copies every
  file. Quetzal does not read `.pteroignore`: rename it.
- **A reply address for the panel's mail, and an `Auto-Submitted` header.**
  Mail from a `noreply@` address that answers nothing reads as less
  legitimate, to a reader and to a spam filter, and an invitation that lands
  in Gmail's spam folder is an invitation nobody accepts. The email settings
  take an optional **Reply-To** (an address, or a name and one, as `from`
  does), and every message now says `Auto-Submitted: auto-generated`, which
  is what keeps an out-of-office reply from answering the panel. The sender
  could already carry a display name — `Quetzal <noreply@example.com>` — and
  it is worth setting: a bare address is one signal more against you.

### Changed

- **A server's CPU is a ceiling, not a reservation.** The CPU set on a server
  was also what it reserved on its node, in use or not: an idle server set to
  six CPUs held them, and the next server to start found no room and stayed
  Pending on a node using a fraction of its processors. Wings reserves none.
  A server now reserves a quarter of its CPU (100m at least), which keeps it a
  fair share of a busy node, and may use up to all of it. Running servers with
  a CPU set restart once, on the upgrade, to take the new reservation.
- **Building Quetzal now needs Go 1.27.** The build image and the CI already
  used it; only the module still declared 1.26, and one of the new confinement
  tests needs 1.27 to pass. Where an extraction's destination directory is
  swapped for a symlink out of the volume mid-upload, Go 1.26 reports the
  symlink as an existing file rather than an escape, so the helper refused the
  operation as an unexpected error instead of a rejected path. Nothing escaped
  on either version; only the status told the caller apart from a fault of ours.

### Removed

- **The one-step import of a server from a Pterodactyl panel is gone.** Giving
  the panel's address and a client API key had Quetzal read a server there,
  fill the create form from it, have the panel produce a backup and stream it
  into the new server's volume. It carried a lot for what it did: an outbound
  client for someone else's panel, its own background job with a heartbeat and
  a retry, a rate limiter, and an import state on every server. Migrating is
  now the manual route the guide already described — create the server from the
  egg's template, then upload its files through the file manager or SFTP, which
  is also the only route that ever worked for a panel on a private address.
  `POST /api/import/pterodactyl/inspect` and
  `POST /api/servers/{id}/import/pterodactyl` are gone, with the `pterodactyl`
  field of a create request and the `import` field of a server; the
  `server.import` event no longer exists, and a notification channel filtering
  on it receives nothing (it was already accepted without matching anything).
  **Importing eggs is untouched** — that is how templates are made, and
  `docs/MIGRATING.md` still starts there. An existing database keeps its
  now-unused `import` column.

### Fixed

- Preserve recovery codes until acknowledged, refresh 2FA policy state without
  reloading, retain unsaved network selections across polling, and reset the
  variables form when switching templates.
- Preserve large JSON numbers and refuse to overwrite malformed JSON/YAML
  configuration files. Template imports accept a UTF-8 BOM and reject
  non-absolute volume paths.
- Make CLI power actions honor hibernation and active offline operations;
  validate managed-database storage changes and serialize database provisioning
  limits and password rotations.
- Cancel pending schedule steps when disabled or deleted, reject task edits
  during an active chain, and prevent stale idle probes from overriding a wake
  or policy change.
- Retain durable backup Job evidence until results are committed. Retry
  uncertain submissions safely, keep retention scoped to the original target,
  and avoid duplicate snapshots or path-dependent retention.
- Freeze file access during transfers, drain writers before copying, and
  finish cancelling destination operations before reopening the source.
  Transfers cannot be cancelled once final source cleanup has begun.
- **A managed database's log is readable again.** Its readiness and liveness
  probes opened a connection to the MySQL port and closed it without
  authenticating, and MariaDB logs a warning for each one: two probes every ten
  seconds filled the log with about 17 000 of them a day, which is where a real
  problem would have been. Both probes now run the image's own
  `healthcheck.sh`, over the local socket as the `mysql` user, which needs no
  credentials, says nothing to the log, and answers a sounder question — the
  port is open while InnoDB is still recovering. Managed hosts restart once,
  on the upgrade, to take the new probes.
- **Eggs that bracket an INI section set their values.** Unreal Engine games
  name their config sections after classes, dots included, and their eggs
  write `[/Script/Engine.GameSession].MaxPlayers` for `MaxPlayers` in the
  section `[/Script/Engine.GameSession]`, as Wings reads it. Quetzal cut the
  key at its first dot: the value went into a section of its own at the end
  of the file, under a name the game does not know, and the setting never
  applied — Satisfactory's player cap, autosave count and connection
  timeouts among them. The stray lines a server got this way stay in its
  file and do nothing; deleting them is safe.

### Security

- Reject uploaded kubeconfigs that reference local credential files or execute
  authentication plugins; refuse to take over another panel's namespaces.
- Confine HTTP file operations and SFTP to a held filesystem root, including
  concurrent symlink changes. File operations now require the Quetzal helper
  image even when SFTP is disabled.
- Disable MariaDB client commands and local-file reads in SQL imports and
  restores. Tenant-configured SMTP cannot connect to private or local addresses;
  the administrator's panel-wide SMTP relay remains usable.
- Consume recovery codes and password-reset links atomically. Password changes
  revoke old reset links, pending email confirmations and obsolete sessions;
  a login using an outdated password proof cannot create a new session.
- Limit sensitive authentication attempts and concurrent password hashing,
  incoming console messages and UDP proxy flows. Existing consoles lose access
  after credential or permission revocation.
- Keep CLI-created secret variables out of public server environments, reject
  zero CPU limits under a CPU quota, and create SQLite files with owner-only
  permissions. See the upgrade guide for previously exposed CLI secrets.
- Update vulnerable web/documentation build dependencies (`source-map-js`,
  `sharp`, `http-cache-semantics` and `postcss-selector-parser`).

## [0.14.0] - 2026-10-05

The server page says which of a server's ports each address reaches — what a
box's port forwarding has to be given — and the file manager creates files.
New managed database hosts run MariaDB 12.3, the current LTS, and Quetzal now
runs and is tested on maintained releases only: Alpine 3.24, PostgreSQL 17
and 18, MariaDB 11.4 to 12.3, Kubernetes 1.37.

**Upgrading from 0.13.0** — one thing behaves differently:

- The images Quetzal pulls on its own have moved on: `mariadb:12.3` (it was
  11.4) for new managed hosts and to dump and load an external host's
  databases, and `alpine:3.24` (it was 3.20) for the install steps that name
  no image. A cluster that cannot reach Docker Hub needs them mirrored. See
  *Changed*.

### Added

- **Each address of a server says which port it reaches.** A server
  published on node ports listed `lolozini.fr:30025, lolozini.fr:30023`, and
  nothing told the voice port from the file transfer one — which is what a
  box's port forwarding has to be given. The server page now reads
  `9987/UDP → lolozini.fr:30025`, `30033/TCP → lolozini.fr:30023`, the address
  to connect to with its port, and the API gives the same in
  `status.portEndpoints`.
- **The file manager creates files.** A config file a game needs — TeamSpeak's
  `ts3db_mariadb.ini` — had to be written elsewhere and uploaded. *New file*
  makes an empty one and opens it in the editor.

### Changed

- **New managed database hosts run MariaDB 12.3**, the current LTS, maintained
  until June 2029; it was 11.4. A host keeps the image it was created with, so
  none changes version on its own; the field takes `mariadb:12.3`, not
  `mariadb:lts`, which would move a host to the next LTS on a restart. The
  backups and imports of an external host use the 12.3 client, tested against
  MariaDB 11.4, 11.8 and 12.3.
- **The generic template and the install steps that name no image run Alpine
  3.24**; 3.20 is no longer maintained. A server keeps the image it runs.

## [0.13.0] - 2026-10-05

A server's databases now go into its backups — each backup dumps them next to
the files, and a restore can load them back — and an SQL dump made elsewhere
can be loaded into one from the server's files. An administrator can also give
a single server a startup command of its own. All three are what moving a
TeamSpeak server onto Quetzal called for: its state lives in MariaDB, and its
egg has no way to say so.

**Upgrading from 0.12.0** — one thing behaves differently:

- A backup of a server that has databases dumps them, and fails when one
  cannot be dumped: a database host that is down now fails the backup it used
  to leave out. The dump runs in the backup Job with a MariaDB image —
  `mariadb:11.4`, or a managed host's own image — which a cluster that cannot
  reach Docker Hub needs mirrored. See *Added*.

### Added

- **Backups take a server's databases along.** A backup copied the server's
  volume and nothing else: a server whose state lives in a database —
  TeamSpeak, a Minecraft server's plugins — lost all of it with the database
  host, backups or not. Each backup now dumps the server's databases, managed
  or external, into its snapshot next to the files, with the server's own
  account (`mariadb-dump --single-transaction`, routines, triggers and events
  included); the backup lists them, and one that cannot be dumped fails the
  backup and says which. A restore can load them back — an option of the
  restore, which takes the databases permission — each emptied first, then
  loaded; a database the server no longer has is named in the restore's
  message. A restore asked without it (the API's default) restores the files
  alone, as before.
- **An SQL dump can be imported into a server's database.** Moving a game's
  database in — TeamSpeak's, a plugin's — took kubectl and a MySQL client of
  one's own. Upload the dump with the server's files, plain or gzipped, and
  load it from the Databases tab (`POST /api/servers/{id}/databases/{dbid}/import`),
  the database emptied first unless asked otherwise. A dump made elsewhere
  fits: the lines that switch to its own database (`--databases`) and the
  definers of a dump taken as root are left out. It runs in a Job with the
  server's own account, on a stopped server that cannot start until it is
  done, as a restore; a failure says where the file stopped, and the
  outcome is the `database.imported` or `database.import-failed` event. It
  takes the files permission as well as the databases one.
- **A server can have a startup command of its own.** The startup came from
  the template alone: an argument its egg has no variable for — the four that
  put TeamSpeak on MariaDB, a JVM flag for one server — meant copying the
  template for that one server, a copy that no longer followed the original.
  An administrator with the servers permission can now write the server's
  command in its settings (`startup` in `PATCH /api/servers/{id}`), with the
  same `{{VARIABLE}}` placeholders, and take it back to the template's in one
  click. Everyone who can see the server's settings sees the command it runs,
  marked *custom* when it is its own; only an administrator changes it, as on
  Pterodactyl. Moving the server to another template drops it, and says so.

## [0.12.0] - 2026-10-05

PostgreSQL can be given field by field — host, port, database, account, a
password kept in a Secret, and TLS checked against the server's CA — rather
than as a DSN, which a password with an `@` or a `/` broke. The unit tests run
on PostgreSQL 14 and 18 as well as SQLite now — and found that servers created
at the same moment there could draw the same node port, fixed here — and the
two-factor setup shows a QR code to scan.

**Upgrading from 0.11.0** — one thing behaves differently:

- The chart refuses, when it is rendered, database settings the panel cannot
  run with — `db.driver=postgres` with neither `db.host` nor a DSN among
  them — where they used to render and leave the pods crashlooping. See
  *Changed*.

### Added

- **PostgreSQL can be given field by field.** A DSN was the only way, and a
  password with an `@`, a `:` or a `/` broke one written by hand. The chart
  takes `db.host`, `db.port`, `db.name` and `db.user`, the password from
  `db.password` or from a Secret of yours (`db.existingPasswordSecret`, which
  can be a CloudNativePG app Secret), and TLS as libpq reads it: `db.sslMode`,
  with the CA that checks the server's certificate in `db.sslRootCertSecret`.
  Outside the chart these are `QUETZAL_DB_HOST`, `_PORT`, `_NAME`, `_USER`,
  `_PASSWORD`, `_SSLMODE` and `_SSLROOTCERT`. Each process names itself to the
  server (`application_name`) and stops trying to connect after 10 seconds. A
  DSN works as before.
- **The two-factor setup shows a QR code.** Adding the account to an
  authenticator app meant typing the setup key, or pasting the otpauth URI
  into an app that takes one, where phone apps expect to scan a code. The
  account page draws the code now, in the browser itself — the secret goes to
  no image service — black on white in either theme; the key and the URI stay
  below it for entering by hand.

### Changed

- **The chart refuses database settings it cannot run with**: PostgreSQL
  with neither `db.host` nor a DSN (`db.dsn` still naming the SQLite file),
  `db.host` with SQLite or with `db.existingSecret`, and `db.sslMode` with a
  DSN, which carries its own. Each used to render, and the panel to crashloop
  on it.

### Fixed

- **Servers created at the same moment on PostgreSQL each get a node port of
  their own.** Two allocations read the same free port and both took it: the
  database's unique index refused the second, and the creation of its server
  with it — the likelier the narrower the `nodePort` pool. SQLite, which runs
  one write at a time, never showed it; the tests on PostgreSQL did. The pool
  now hands out one port at a time.

## [0.11.0] - 2026-10-03

A full test pass of 0.10.0 turned up thirty findings, and this release
answers them. A server's network policy is in place before anything of its
tenant runs, and the first-run setup asks for a code from the panel's log. A
restart gives the game its stop command, a restore can no longer be left
waiting under a running server, and a schedule's chain survives the controller
restarting. What SFTP sessions change now shows in a server's activity, and an
email address is confirmed by a link mailed to it.

**Upgrading from 0.10.0** — several things behave differently:

- Game servers run in the panel's time zone, the controller's `TZ` (UTC when
  unset), each from its next start. See *Changed*.
- A template's update reaches a running server at its next restart, and its
  status says a newer version waits. See *Changed*.
- A new install's first-run setup asks for the setup code the apiserver
  prints in its log; an install already set up sees nothing of it. See
  *Security*.
- A rename onto a name already taken is refused. See *Security*.
- A restart is a stop, with the template's stop command, then a start: it
  takes as long as the game does to stop. See *Fixed*.
- New usernames are ASCII letters, digits, dots, dashes and underscores; an
  email address is one account's, and one given from the account page waits
  for its owner to confirm it when the panel can send mail. Existing names
  and addresses are left as they are. See *Added* and *Fixed*.
- Inspecting a Pterodactyl server takes the right to create servers, and an
  account gets 30 inspections and 10 test mails an hour. See *Fixed*.
- A data manager keeps the SFTP server it runs until it restarts, which
  turning SFTP off and on does: until then, it lets a key in under any name
  and records no SFTP activity. See *Added* and *Fixed*.

### Added

- **What SFTP sessions change shows in the server's activity.** An upload, a
  deletion, a rename or a new folder made over SFTP was recorded nowhere a
  server's owner could see, where the file manager's own changes are in the
  activity feed. The SFTP container's log has a line for each change now
  (see *Fixed*), and the controller reads it into the server's activity,
  under the account that made each change, as `sftp.write`,
  `sftp.delete`, `sftp.rename`, `sftp.mkdir`, `sftp.symlink` and
  `sftp.link`, which a notification channel can also follow. Many changes
  of one kind made together read as one entry — a folder of a thousand
  files is one line, naming the first few. A data manager keeps the SFTP
  server it started with across an upgrade, and 0.10.0's logs no changes:
  its server's SFTP activity starts when it restarts, which turning SFTP off
  and on again does.
- **An email address is confirmed by a link mailed to it.** Anyone could give
  their account any address, someone else's included, which then held it:
  the address's owner could not use it for an account of their own. A new
  address now waits for its owner to open a link mailed to it, valid for a
  day, and the account keeps the address it had — and its password resets —
  until then, so a typo costs nothing. The account page shows whether the
  address is confirmed, and can send the link again or drop the change. An
  address an administrator writes, or one given while the panel cannot send
  mail, is taken as given and shown unconfirmed; an invitation's is confirmed
  by the invitation.

### Changed

- **Game servers run in the panel's time zone.** They ran in UTC whatever
  the panel's `TZ`: a game in Paris logged 12:36 at 14:36, and a plugin's
  daily restart or timed message came two hours off. A server gets the
  controller's zone, as Wings gives its host's, UTC when none is set. A
  server already running keeps UTC until its next start, rather than every
  server restarting with the upgrade.
- **An updated template reaches a running server at its next restart, not at
  once.** Replacing or editing a template replaced the pod of every running
  server that used it, within seconds and with players on, where
  Pterodactyl's panel applies an egg's changes at a server's next start. A
  server whose game is up now keeps the version it started with, and its
  status says a newer one is waiting; it takes the new version when its pod
  goes anyway — a stop, a restart, hibernation, a crash, a change to its own
  settings — or when it is reinstalled. Earlier versions are kept only while a
  server runs them.
- **The administration is in tabs, and a phone shows every tab.** Its eleven
  cards made one page over 5,000 pixels high with nothing to find one by;
  each has a tab and an address of its own now (`#/admin/users`,
  `#/admin/templates`…), as a server's page does. On a phone, tabs that do
  not fit go onto a second line, where they used to scroll sideways behind a
  hidden scrollbar. And a server's power buttons are offered when they do
  something: Start was live next to a running server.

### Fixed

- **Restart, and any change that replaces a server's pod, give the game its
  stop command first.** Only a stop did: a restart — from the panel, the API
  or a schedule — deleted the pod, and a new setting (memory, image,
  variables, ports) or a reinstall replaced it, with SIGTERM alone, which a
  startup wrapped in a shell never passes on. A game that saves only on its
  stop command lost everything since its last autosave at every scheduled
  restart. A restart is now a stop followed by a start: the stop command,
  the pod gone, then the server started again — where the next pod used to
  start on the volume while the old one was still saving to it. A pod
  replaced for a new setting gets the stop command just before.
- **A restore can no longer be left waiting under a running server, to roll
  its world back later.** A restore waits for its server's pods to be gone,
  and starting the server after asking for one was accepted: the restore then
  waited — days, on a server left running — with the file manager and SFTP
  down all along, could not be cancelled, and ran the next time the server
  stopped, over everything played since. While a restore waits or runs, the
  server cannot be started now, by hand or by a schedule, nor transferred or
  imported into, and the file manager says why at once instead of after two
  minutes. A restore still waiting can be cancelled from the backups, and one
  whose volume is still held 15 minutes after the request is called off and
  reported as failed.
- **A schedule's chain survives a restart of the controller.** A chain ran
  in the controller's memory: restarted during a delay or a backup — by an
  upgrade, a rollout, a node drain — it dropped the rest, and a nightly
  stop, backup, start left the server stopped until the next night, with
  nothing said. How far a chain has got is kept now, and the next
  controller carries it on where it was, its delay kept and its backup
  waited for rather than taken again; one left more than a day is closed
  without running, and disabling the schedule drops it.
- **A template without variables no longer blanks the panel.** Its variables
  went out as `null`, and the create form, which reads a list, crashed: the
  whole panel turned into an empty page, for every account as soon as such a
  template came first in the list. They go out as an empty list now, the
  forms cope with a missing one, and a page that fails to render shows the
  error with a way back instead of taking the panel down.
- **A node port that another Service already holds is replaced.** The pool
  only knows Quetzal's own allocations, and draws by default from the
  cluster's whole range, which an ingress controller's LoadBalancer Service
  or any other NodePort Service draws from too: the cluster then refused the
  server's Service on every pass. Quetzal now sets that port aside for good,
  publishes the server — or its SFTP — on another one in the same pass, and
  records a `server.port-moved` event, which notification channels can
  select, since the address players use changes.
- **A server keeps its node ports while it exists.** Taking a server off
  NodePort gave its ports back to the pool, and putting it back drew new
  ones — 30003 became 30027, and 30003 could go to the next server: the
  address its players knew, a box's port forwarding and an SRV record all
  to redo. A port is freed when it is removed from the server, or the
  server deleted.
- **A server's status says what the cluster refused.** A step of the
  reconcile that failed kept the status from being written, so the panel
  went on showing what an earlier pass had found: *Stopped*, without a
  message, next to a game that was running. The status now follows what
  runs, and its message gives Kubernetes's answer — the Service refused for
  a port already allocated, say. Connection errors are left to the
  controller's log, since they name the cluster's address.
- **A crash says what the game's log ends with.** A Paper server out of heap
  prints `java.lang.OutOfMemoryError` and exits 0, and its status read "the
  game exited with code 0", which sent nobody towards the memory. The
  message quotes the last error line of the run's log now, and says to give
  the server more memory when that line is an out-of-memory error.
- **A server's page shows a subuser what they were given, and nothing
  else.** It was drawn for the owner whoever opened it: a subuser given the
  files had no Files tab — only SFTP worked — and none could reach the
  databases or the settings they were given, while one without power saw the
  power buttons and the delete card, which answered 403. A server now comes
  with `myPermissions`, what the reader may do on it, and the page follows
  it: tabs, power buttons, settings, SFTP switch, delete. Access and the
  choice of template stay with the owner and the administrators.
- **A variable's value is checked against its egg's rules.** Quetzal kept an
  egg's validation rules but checked only that a required value was there and
  that a choice was one of the list: Paper's jar name took
  `foo bar; echo pwned` and its version `not a version at all !!`, and the
  server failed at its next start. A value is now refused when it is given,
  with what the rule asks, for the rules eggs use: `regex`, `max`, `min`,
  `between`, `size`, `integer`, `numeric`, `boolean`, `in`, `digits`,
  `alpha_dash`, `url`, `email`, `ip` and the like. A pattern RE2 cannot read
  is let through rather than refused. Switching template resets a value the
  new template's rules refuse to its default, and says so.
- **Usernames are told apart, and kept readable.** Any name of three
  characters or more was taken: next to the superadmin `Lolozini` came
  `lolozini` and `LOLOZINI`, names with spaces, HTML, control characters and
  a newline, and one of 204 characters that PostgreSQL would have refused
  with a 500 — names that then read as someone else's in access lists,
  activity and notifications. A new name is ASCII letters, digits, dots,
  dashes and underscores, starting with a letter or a digit, 3 to 64 of
  them, at setup, by an administrator and from an invitation alike; a name
  another account has, case aside, is taken; and signing in finds the
  account whatever the case typed. Existing names are left as they are.
- **An email address belongs to one account.** Any account could set any
  address, another account's included, and a password reset by address went
  to the oldest of the accounts that had it: one account could divert
  another's resets. An address another account has is refused now — on the
  account page, by an administrator, and from an invitation, whose reader is
  asked to sign in to the account that has it — and an address two older
  accounts still share resets neither, until one changes it; both can still
  be reset by name. An address is also confirmed by mail now: see *Added*.
- **SFTP lets a key in under its account's name only, and logs what it is
  used for.** The SFTP server took any name with any key it knew, so the
  name a session gave said nothing about whose key it was, and nothing
  recorded what a session changed. Each key in a server's authorized_keys
  now carries the accounts it belongs to, and a session signing in under
  another name is refused; the SFTP container's log has a line for every
  write, removal, rename, new folder and link, with the account that made
  it. A data manager still running the previous release's SFTP binary
  accepts any name until it restarts.
- **Requests the panel makes on a caller's word are bounded.** Inspecting a
  Pterodactyl server has the panel call an address the caller gives, and
  any signed-in account could, without limit, even one allowed no server; a
  test mail goes from the operator's domain to any address, and a settings
  administrator could send them without end. An account that may create no
  server cannot inspect one any more, and each account gets 30 inspections
  and 10 test mails an hour.
- **An invitation to an address the mail server refuses says so.** It
  answered that the invitation could not be sent and that an administrator
  could check the email settings, which were fine: it was the address. A
  recipient the relay refuses is told apart from a relay that failed, and
  the inviter is told the server refused that address, with its reply.
- **Large directories list in a blink.** A listing ran `stat` and `wc` for
  each entry, about 2 ms apiece in a server's pod: a directory of 3,000
  files took six seconds, and one of 30,000 — playerdata, a plugin's cache —
  outlasted the request and could not be listed at all. Where the game's
  image has GNU find, as the Debian and Ubuntu images most eggs use do, one
  `find` reads the whole directory now: 30,000 entries in about 150 ms. An
  image with busybox alone is still gone through an entry at a time.
- **Accented and non-Latin text comes through the console whole.** The log
  was read in 4 KiB blocks and each sent as text, and a character of two to
  four bytes that a block ended in the middle of turned into two `�` —
  accented chat, Cyrillic or CJK logs had holes in them where the pod's log
  had none. A block is cut between characters now.
- **An unknown API route answers in JSON.** Every error of the API is
  `{"error": …}` but the ones no route took, answered "404 page not found"
  in plain text; a known route called with another method still answers
  405, with the methods it takes.
- **The SFTP card no longer says it works only while the server runs.** SFTP
  runs in the data-manager pod, which is up whether the game is or not: the
  card discouraged the very use it is for, putting a world in place before
  the first start.
- **The upgrade guide no longer promises a backup Quetzal does not take.** It
  said Quetzal could back the panel's database up to the S3 target, and an
  operator could skip the snapshot of the one thing that is the source of
  truth; nothing does that. It now shows how to copy the SQLite database out
  with the panel stopped, in a pod the `restricted` Pod Security level
  admits. The README no longer says a deleted server's snapshots stay in the
  bucket — they are purged — nor that a server is one pod; the install guide
  says the chart is one Deployment of two containers, that it needs
  Kubernetes 1.30 and is tested on 1.35, and that SFTP runs in the
  data-manager pod, not the game's.

### Security

- **A server whose Service the cluster refuses no longer runs without its
  network policy.** The policy that keeps a game's code — a tenant's mods and
  plugins — off the cluster network was written last, after the server's
  Service, and a Service the cluster refused stopped the pass before it, on
  every pass. A node port that a Service outside Quetzal already held was
  enough: the game could reach the panel, the Kubernetes API and the other
  servers, while the panel showed it *Stopped*. The policy now goes in right
  after the namespace, and nothing that runs a tenant's code is created or
  changed while it cannot be written.
- **The first account needs a code from the panel's log.** Until someone had
  made it, the first-run setup made a superadmin of whoever reached the
  panel first, and the install guide publishes the panel on an Ingress
  from the start. The setup asks for a setup code now, which the panel
  prints in its log until the account exists; the chart's notes and the
  install guide say how to read it. An install already set up sees nothing
  of it.
- **A rename can no longer move a file out of the server's data directory.**
  Renaming onto a symbolic link to a folder put the file inside the link's
  target, as `mv` does with a folder: through a link aimed outside the data
  directory, the file left the volume for the data manager's own
  filesystem, past the guard, which allows a link as the last part of a
  destination. A rename onto a name already taken — a file, a folder, a
  link — is refused now, as moving files already was, which also stops a
  rename from overwriting a file without a word.

## [0.10.0] - 2026-10-02

Uploads of any size: the file manager sends files and archives in pieces that
each fit within the read timeout of the proxy in front of the panel, shows
their progress, and resumes one that was interrupted. A write the symlink
guard refuses also answers at once, where it held the request for an hour.

**Upgrading from 0.9.0** — one thing behaves differently:

- An upload in progress, or interrupted, shows as
  `<name>.quetzal-part-<id>` beside its destination (`.quetzal-upload-<id>`
  inside an archive's directory) until it finishes, is cancelled, or is
  collected 24 hours after its last piece. See *Added*.

### Added

- **Uploads of any size get through the proxy in front of the panel, and
  resume.** A file went up in one request, which lasted as long as the
  transfer, and proxies give a request a fixed time to arrive: behind
  Traefik 3, whose limit is 60 seconds, anything over a few hundred
  megabytes on a home connection was cut and lost whole, as was an archive
  taking more than 15 minutes. The file manager now sends files and archives
  in pieces sized to take a few seconds each, with a progress bar and a
  cancel button. A piece that fails is sent again from where the server says
  the upload is, and choosing the same file again after an interruption
  resumes it, for 24 hours. The file replaces its destination in one step
  once every byte has arrived; an archive is unpacked then. Uploads are no
  longer bounded at 256 MiB for a file and 2 GiB for an archive, but at
  64 GiB. The API has the same, under `/api/servers/{id}/uploads`; the
  one-request endpoints stay. Behind ingress-nginx, raise its body size
  limit to 32 MiB: see *Behind a proxy* in the install guide.

### Fixed

- **A write or an archive the panel refuses answers at once instead of
  hanging.** Writing a file, or extracting an archive, through a symbolic
  link that leaves the data directory was refused by the guard before the
  script read the request body, and the container runtime then held the
  exec open until that body was consumed: the request hung until its
  timeout, an hour for a write, and answered 502. The scripts now read what
  is left of the body on their way out, and the refusal comes back as a 400
  straight away.

## [0.9.0] - 2026-10-02

Invite people by email: a server's owner types an address where a username
went, and the link that arrives lets its reader accept from their account or
create one there. The mails the panel sends people — the password reset, the
invitation, the settings test — now carry Quetzal's colours. And the control
plane's account loses the last rights that reached past its own namespaces:
patching any volume, deleting any binding, relabelling any namespace.

**Upgrading from 0.8.0** — three things behave differently:

- A server's owner can invite someone by email, and the invitation can create
  an account on the panel, one that owns nothing and reaches only the servers
  it is invited to. To keep account creation to administrators, turn off
  *Admin → Invitations*. See *Added*.
- `PUT /api/security-settings` changes only the fields it names: a body
  without `requireTwoFactor` leaves the two-factor policy as it is, where it
  used to turn it off. See *Changed*.
- A remote cluster keeps the broader rights it was given until the manifest
  the panel shows is applied there again. See *Security*.

### Added

- **Invite someone to a server by email.** Pterodactyl's way of adding a
  subuser, and the only one for someone who has no account yet: the access
  form now takes an email address as well as a username. The address gets a
  link, good for 7 days and once, that accepts the invitation from the account
  its reader signs in to, or from one they create there. Such an account may
  own no server until an administrator allows it, and its email is the invited
  address. The address is never matched against accounts: an account's email
  is not verified, so listing someone's address in a profile gains nothing.
  The owner sees the invitations waiting and can withdraw them. Administrators
  can stop invitations from creating accounts (*Admin → Invitations*), which
  leaves them to people who already have one. Needs the panel's email and
  public address; at most 50 open per server and 20 sent per hour by each
  account.

### Changed

- **The password reset mail, the invitation and the email settings' test
  mail are in Quetzal's colours**, with the logo, a button for the link, the
  address in full below it, and a text version for clients that show no HTML.
  Cream by default and the panel's dark theme for a reader in dark mode; the
  logo travels inside the message, so it shows without allowing remote
  images. Event notifications are unchanged.
- **`PUT /api/security-settings` changes only the fields it names**, now that
  it holds two: the two-factor policy and whether invitations create
  accounts. A body without `requireTwoFactor` used to turn the policy off. A
  request refused for one field changes neither.

### Security

- **The control plane's account no longer reaches past its own namespaces
  through volumes, bindings or namespace labels.** It could still patch any
  PersistentVolume on the cluster, a right left over from the "keep data"
  option that 0.2.0 removed: a volume's claim reference decides which claim
  mounts it, so a compromised panel could have mounted another application's
  data. It could also delete any RoleBinding, relabel or create any namespace,
  and, in its own namespaces, bind its role to someone other than itself. The
  chart and the remote-cluster manifest drop the volume rule and keep only
  *create* on RoleBindings, and the admission policy now also refuses
  namespace writes outside Quetzal's namespaces and any binding of another
  role or to another account. Remote clusters pick this up when the manifest
  the panel shows is applied again.

## [0.8.0] - 2026-10-01

Schedules do what their chain says: a backup step ends when its backup does,
and a backup waits for a stopping server to be down, so *stop → backup →
start* copies a stopped world and *save-off → backup → save-on* a quiet one.
Every run is in the activity log. A game that never prints its done line is no
longer shown Starting for half an hour at every start, a deleted SSH key's
SFTP sessions close within seconds, and backups run restic 0.19.1.

**Upgrading from 0.7.0** — three things behave differently:

- A schedule's backup step now waits for its backup, and fails when the backup
  does: a chain stops there unless the step continues on failure, and the
  steps after it run once the backup is over, not as soon as it was
  requested. A backup asked for while its server is stopping starts once the
  game is down. See *Changed*.
- A file path that climbs above a server's files, such as `../config.yml`,
  answers 400 where it was quietly brought back inside them. See *Changed*.
- Backups run restic 0.19.1 unless the backup settings name another image.
  See *Changed*.

### Added

- **A schedule's runs are recorded.** What the scheduler did showed only as
  the schedule's last status, which the next run overwrote, and nowhere in
  the activity log: a server restarted at four in the morning left no trace
  of why. Each run is now an entry of the panel's activity log and of the
  server's, `schedule.run`, saying what each step did. A notification channel
  receives it only when it lists it, since a schedule can run every minute.

### Changed

- **A backup step in a schedule ends when its backup does.** It ended when the
  backup was requested, so *stop → backup → start* started the server again
  while its backup was still being taken, a copy of a running game, and a
  chain that paused the game's saves for a backup resumed them before
  anything had been copied. The next step now waits for the backup, and the
  step fails with it: a nightly backup that failed read "ok". A backup of a
  server that is stopping also waits for its game to be down, up to 15
  minutes, so stopping a server and backing it up copies a stopped world. To
  copy a Minecraft world without stopping it: `save-off`, `save-all flush`,
  the backup with "continue on fail", then `save-on`.
- **A game that does not print its done line is not shown Starting for half
  an hour at every start.** Counter-Strike 2 without a valid game server
  token takes players but never prints "Connection to Steam servers
  successful": each start showed it Starting for 30 minutes before it was
  reported Running, and the message explaining why was gone a few seconds
  later. After a start that went without its done line, the next ones are
  reported Running as soon as their container is up, the message stays while
  it runs, and the line is still looked for: once it shows, the usual wait is
  back.
- **A file path that leaves the server's files is refused.** `../escape.txt`
  was brought back inside them without a word, and the file was written as
  `escape.txt` at the top of the server's files, which is not what was asked
  for. Every file route now answers 400 for a path that climbs above the data
  directory; `..` that stays inside it still works.
- **The default backup runner is restic 0.19.1**, where it was 0.17.3. Since
  0.18, restic no longer fails a backup over a file that disappears while it
  runs, which a running game does all the time. Repositories stay as they
  are, and a runner image set in the backup settings is kept: it must still
  be restic 0.17 or later.
- **A transfer names its destination cluster by slug.** Creating a server on
  a cluster took the cluster's slug (`cluster`), and moving one there its
  numeric ID (`targetCluster`), which a script had to look up first.
  `POST /api/servers/{id}/transfer` takes `cluster` now; `targetCluster` is
  still accepted.
- **A cluster registered at a loopback address says why it is unreachable.**
  The script that prints the kubeconfig to register takes the cluster's
  address from the operator's kubectl context, often 127.0.0.1 through a
  tunnel or to a kind cluster, which from Quetzal leads back to Quetzal. The
  script now warns about such an address, the form says to check it, and a
  failed connection to one says what is wrong.
- **An admin role says what each of its permissions allows.** The role form
  listed them by name, with a tooltip in English, and nothing said that
  managing accounts includes resetting a user's password, which opens that
  user's servers. Each permission is explained under its name now, in the
  panel's language, and the account page and the API reference say that an
  API key signs in without the second factor.

### Fixed

- **A file whose name holds a newline is listed whole.** The game or a plugin
  can create one, and the file manager listed `new\nline.txt` as `new`, a file
  that was not there, while the real one could be neither renamed nor
  deleted from the panel.
- **An email server set to the wrong TLS mode for its port says which to
  choose.** Set to STARTTLS, a server on port 465, which expects TLS from the
  first byte, kept a send waiting 20 seconds before "i/o timeout". It fails
  after 10 seconds now and says to choose implicit TLS, and a server that
  greets in plain text under implicit TLS says to choose STARTTLS. The email
  settings and email channels point out either mismatch as it is typed.
- **The API reference says what the API answers.** Requesting a backup or a
  restore answers 202, where it said 201; saving the backup settings may
  answer 200 with a warning, and cancelling a transfer already cancelled 200;
  `GET /api/clusters/setup-manifest` is described. One name given to the TCP
  and UDP entries of a port is refused with how to name them.

### Security

- **A deleted SSH key's SFTP sessions close within seconds of the key being
  refused.** A key reaches the SFTP server a minute or two after it is added
  or deleted, through a ConfigMap the kubelet refreshes on its own clock, and
  open sessions were then checked every 30 seconds: a deleted key's session
  could stay open two minutes after the click. They are checked every 5
  seconds now, and the account page says how long a key takes to be accepted
  or refused.

## [0.7.0] - 2026-10-01

A server's page in tabs, a panel that fits a phone, and game servers held more
tightly. The console now opens a server's page, a server can be given other
servers to reach — a Velocity or BungeeCord proxy and the servers behind it —
and its image switched without a reinstall. Sleeping servers no longer wake for
port scans and server-list queries. Game servers reach DNS through the
cluster's resolver only, an install runs within its server's limits, and the
game's container gets Wings' memory headroom.

**Upgrading from 0.6.0** — six things behave differently. Game servers keep
running through the upgrade: the new install limits and memory headroom reach
each one at its next stop and start.

- Game servers resolve names through the cluster's DNS only: its CoreDNS or
  kube-dns pods, a node-local cache at a link-local address, or the resolver
  the controller was given. If your pods use a resolver elsewhere, add its
  address to `egressAllow`. See *Security*.
- Importing an egg over a template of the same slug answers 409: a script
  that re-imports eggs to update them needs `?ifExists=replace`. See
  *Changed*.
- `GET /api/servers/{id}/stats` answers 200 with `available: false` for a
  server with nothing to measure, where it answered 409 or 503. See
  *Changed*.
- A notification channel no longer receives `notification.*` events unless it
  lists them, and one whose filter names an event type the panel does not
  record must drop it the next time it is saved. See *Changed* and *Fixed*.
- A custom backup runner image must be restic 0.17 or later. See *Fixed*.
- An SSH key added twice to one account keeps its oldest entry. See
  *Security*.

### Added

- **A server can reach the servers it is given.** A game server cannot reach
  the others inside the cluster, so a Velocity or BungeeCord proxy could be
  joined to its servers only through the internet, where the servers behind
  it, which trust the proxy to have checked the players, could be joined
  directly. A server's settings now list the servers it may reach, with the
  address to give the proxy for each; they need not be exposed at all. Only
  someone who may change both servers can link them, and only on one cluster.
- **A server's image can be changed from its settings.** Going from Java 21
  to Java 25 took a reinstall, which ran the install script again and
  downloaded the game once more; Pterodactyl makes it a choice in the startup
  settings. A server's Settings tab now offers its template's images, and
  `PATCH /api/servers/{id}` takes `image`, with the rule of the creation form:
  the template's images, or any for an administrator. Like the other
  settings, it restarts a running server.
- **An email channel can send through the panel's email settings.** It
  needed an SMTP server and a sender of its own, typed in again although the
  panel already sends its password resets through one. A channel that names
  only its recipients now uses the panel's server, and its sender unless it
  sets one.

### Changed

- **A port scan or a server-list query no longer wakes a sleeping server.**
  Outside Minecraft Java, anything woke one: a scanner's bare TCP connection,
  a Bedrock client refreshing its server list, a tracker's Steam query. On the
  internet, that was all the time. A TCP connection that hangs up without
  sending a byte, and a UDP packet that only asks about the server (Steam's
  A2S queries, RakNet's ping, the GameSpy query), now wake nothing and do not
  keep a server awake either; anything else still wakes it, so no game's
  player is kept out. A sleeping Bedrock server also shows as asleep in the
  server list, where it looked offline, once it has answered the list at
  least once. An activator in wake-and-drop mode takes the new rules with
  the upgrade, and one in proxy mode the next time its server sleeps: until
  now an activator kept the version it started with for as long as it ran.
- **A server's page is in tabs, and opens on its console.** It was one page,
  7,000 to 10,000 pixels high, with the console at the very bottom, below the
  files, backups, schedules, databases, access and settings. The console now
  comes first, under the server's address and power buttons, with its resource
  charts below it; files, backups, schedules, databases, access, settings and
  activity each have a tab, and an address of their own that a reload or the
  back button returns to. The server's exposure, hibernation and transfer
  moved to its Settings tab, and so did deleting it, which sat at the top of
  the page.
- **A new server is exposed on a node port by default.** The create form
  offered ClusterIP, which only the cluster itself reaches, so a server
  created without touching the form was one no player could join. It starts
  on NodePort now, and an administrator who leaves the memory limit blank is
  told the server may then take all of its node's memory.
- **A server with nothing to measure answers its stats with 200.** A
  stopped, installing or sleeping server, or one on a cluster without
  metrics-server, answered `GET /api/servers/{id}/stats` with a 409 or a
  503, which the panel, polling every four seconds, turned into a red error
  in the browser console each time. It answers 200 now, with `available:
  false`, a `reason` and the server's limits. The panel shows those limits
  when there is no usage to show next to them, where it said "Resources —",
  and the sign-in form tells password managers which password it wants.
- **A server's Access tab says what each permission allows.** It listed
  their names alone, which left it to guess that `view` also shows the
  server's backups, schedules and activity, while its console, files and
  databases each need their own. Nothing changes in what they allow; the
  install guide lists them too.
- **Notifications say what happened.** A mail's subject and a Discord
  embed's title were the event's type, "Terraria — server.power"; they read
  "Terraria — Power action" or "Server crashed" now, with the type still in
  the mail's body and under the Discord embed. A channel is no longer told
  when another channel is created, changed or deleted, unless it lists
  `notification.*` events: setting channels up pinged all the others.
- **A server's container gets Wings' memory headroom.** Its memory limit was
  the memory set, exactly, so an egg starting Java with
  `-Xmx{{SERVER_MEMORY}}M` had a heap as large as the container and was
  killed once the heap filled. The container may now use 15 % more than the
  memory set up to 2 GiB, 10 % up to 4 GiB and 5 % beyond, as under Wings;
  what the node holds for the server, `SERVER_MEMORY` and quotas stay the
  memory set. A running server takes it at its next stop and start, not with
  the upgrade.
- **Importing an egg no longer replaces a template of the same name without
  asking.** Two different eggs can share a name, and so a slug: Pterodactyl's
  Paper imported over Pelican's replaced it, and the servers created
  afterwards lost Java 25. Such an import is now refused with a 409 that
  names the template in the way and the servers using it; the panel asks
  whether to replace it or to add the egg beside it (`paper-2`), and the API
  takes `?ifExists=replace` or `?ifExists=copy`. A script that re-imports an
  egg to update it needs `?ifExists=replace`.

### Fixed

- **A database on an external host at a private address is reachable from
  the servers.** A game server may not reach private addresses, and only a
  host given by a literal address got a way through, so one named by DNS --
  a MariaDB in the cluster, or on the LAN -- handed its users an address that
  never answered. A Service of the cluster named in full
  (`<service>.<namespace>.svc.cluster.local`) is now reached through its
  namespace, and any other name through the addresses it resolves to, looked
  up again every minute.
- **A Bedrock player coming through Geyser wakes a sleeping Java server.** On
  a Minecraft Java server nothing on UDP woke it, since that was its query
  port, so a Bedrock player could not wake it, and a server with only Bedrock
  players on it could fall asleep under them.
- **The panel fits a phone.** At 390 pixels wide nearly every page scrolled
  sideways: a table set the width of the page, the file manager's buttons ran
  off its card (on a desktop too), and the top bar stacked its buttons on
  three lines. Tables now scroll inside their card, a file's actions open
  below it from its ⋯ button, and on a narrow screen the top bar takes two
  lines, the folder tree and modification dates give way, and paired fields
  stack.
- **The create form no longer takes a port variable for an egg's game port.**
  An egg that hands the game its allocation (`-port {{SERVER_PORT}}`) has no
  variable for the game's port, and the first port variable was taken for it:
  Counter-Strike 2's SourceTV port, 27020, where SourceTV also listens, on TCP
  only, so nobody could join. The form now asks for the game's port, on TCP
  and UDP as Wings exposes an allocation, suggests the egg's port variables
  the same way, and fills in 25565 on TCP for Minecraft Java only. An egg with
  no port, a chat bot say, gets none.
- **An egg's yes/no variable keeps the egg's 1/0.** The create form offered
  true/false for every boolean variable: one set to 0 showed "true", and
  choosing "false" sent a value that a script testing for "0" took as on, so
  Counter-Strike 2's RCON came on when turned off.
- **A sender written "Name <address>" sends.** The email settings and email
  channels saved one without a word, then the relay refused every message,
  password resets included: the whole string went into the SMTP envelope.
  The envelope takes the address now, and the From header keeps the name. A
  sender that is not an address at all is refused when it is saved.
- **A new install says it has no template.** Built-in templates stopped being
  installed in 0.2.0, but the README still listed them, and the create form
  of a new install was an empty picker that said nothing. It now explains
  that servers are made from imported eggs, and takes an administrator
  straight to the import; the README and the install guide say so too.
- **A backup that cannot reach its target says why, and fails sooner.** The
  error of the job's first command was thrown away, the job then tried to
  create the repository, and it failed with restic's "create repository ...
  failed" whatever was wrong: a refused connection, a refused key, a wrong
  password, a missing bucket. A target that dropped the traffic was waited
  for twice, 90 seconds each time. The repository is now created only when
  restic says there is none, and any other failure ends the run, said as what
  to check: the object store refused the connection or did not answer, its
  name does not resolve, it refused the keys, the bucket does not exist, the
  password does not open the repository. A custom runner image must be
  restic 0.17 or later.
- **Turning hibernation on no longer puts an idle server to sleep at once.**
  The idle countdown ran from the server's last activity, however long ago,
  so a server nobody had joined for a day went to sleep seconds after
  hibernation was turned on. It starts when hibernation is turned on now, and
  again whenever its settings change.
- **The published host must be one players can reach.** Anything was saved
  as the host shown in front of every server's port, and "bad host!" gave
  players "bad host!:30158". The panel-wide and per-cluster hosts are a DNS
  name or an IP address now, and an IPv6 address is shown in brackets
  (`[fd00::1]:30158`), where it read as another address.
- **Deleting an account says who gets its servers.** They go to the
  administrator who deletes it, while the confirmation only said they were
  not deleted. It now counts them and names who they go to; the user list
  carries each account's number of servers (`servers`).
- **A database host's state is checked without anyone asking.** It was
  checked only when an administrator pressed "test": a managed host whose
  MariaDB had been ready for minutes read unreachable, and an external host
  that went down read reachable until somebody looked. The controller now
  checks every host, a reachable one every five minutes and one that is not
  every thirty seconds, and an external host as soon as it is added or
  changed. The list says whether a host is reachable, unreachable (and why,
  on hover) or not checked yet.
- **A notification channel is checked when it is saved.** A filter on events
  the panel never records was accepted and then received nothing, and a
  webhook at `gopher://` or `file://` was refused only when the first event
  went out. An unknown event type and a url that is not http(s) are refused
  with a 400 now, and `GET /api/notifications/event-types` lists the types.
  One of them, `server.stopped`, was offered by the panel and never recorded:
  a server that goes down now says so.
- **SFTP's `symlink` makes the link the client asked for.** OpenSSH sends a
  link's target before the link itself, and the two were read the other way
  round: `symlink /etc qa/link` made a link named `etc` at the root, pointing
  at `qa/link`. The link is now where it was asked for, pointing inside the
  server's files as before.

### Security

- **An external database host opens only its database port to the servers.**
  A host given by a literal private address let the servers with a database
  on it reach every port of that address, the cloud metadata endpoint
  included had a host been pointed at it. It is the database port only now,
  and link-local and loopback addresses never get through.
- **A game server's DNS goes to the cluster's resolver only.** The policy
  opened port 53 of every address to the servers: the LAN's router and
  domain controllers, anything in the cluster listening there. They may query
  the cluster's CoreDNS or kube-dns pods, a node-local cache at a link-local
  address, and the resolver the controller was given, and public resolvers
  as before; nothing else on port 53. A cluster whose pods are pointed at a
  resolver elsewhere needs its address in `egressAllow`.
- **An install script runs within its server's limits.** The install
  container had none, so an egg's script could take all of its node's
  memory and CPU, and the helpers that run before a server starts had none
  either. The install gets the server's limits, with at least 1 GiB and one
  CPU as Wings gives an installer, and asks the node for no more than the
  server does; the helpers get small fixed limits. A running server takes
  them at its next stop and start, not with the upgrade.
- **Deleting an SSH key revokes it.** The same public key could be added to
  an account twice, and deleting one of the two left the key working through
  the other: an SFTP session stayed open after its key was deleted. A key is
  on an account once now, and adding it again is refused with a 409 that
  names it. The upgrade keeps the oldest of a key an account holds twice and
  removes the others. Another account may still add the same key: SFTP logs
  into the account its username names.
- **A failed backup no longer gives away the object store's address.** A
  backup's message is readable by anyone who can see the server, and the
  backup target by administrators only. The repository's URL was taken out of
  it, but not the object store's own URL and address, which restic gives when
  it cannot reach the bucket.

## [0.6.0] - 2026-09-30

New accounts closed until an administrator opens them, and five security
fixes. An account created without quotas could create as many servers as it
liked; a new one now creates none until it is given some. Someone guessing at
the administrator's password no longer locks them out of the panel, a
suspended server is frozen for its owner, who could delete it before anyone
looked into it, and requiring a second factor no longer locks out the
superadmin who requires it. Backups record where they went, and the panel says
when a node goes down.

**Upgrading from 0.5.1** — four things behave differently. Game servers keep
running through the upgrade.

- In a quota, `0` now means none and `-1` unlimited, and a new account may own
  no server until an administrator gives it some. Existing accounts are
  converted once and keep what they could do; a script that sets quotas
  through the API must send `-1` where it sent 0. See *Changed*.
- A server created by anyone but an administrator needs a memory limit. See
  *Changed*.
- The owner and subusers of a suspended server can only look at it: anything
  else answers 409, file requests included, which answered 403. See
  *Security*.
- A superadmin needs a second factor of their own before requiring one. See
  *Security*.

### Changed

- **A new account creates no server until an administrator allows it.** An
  account created without quotas could create as many servers as it liked:
  0 meant unlimited. It now means none, `-1` means unlimited, and a new account
  starts with no servers and no bound on memory or CPU, so giving it a number
  of servers is what opens it. The Users card shows unlimited as an empty
  field and can now change an account's quotas after it is created. Existing
  accounts keep what they could do: each quota of 0 becomes unlimited, once,
  when the panel upgrades. A script that sets quotas through the API must send
  `-1` where it sent 0 for unlimited.
- **A server created by anyone but an administrator needs a memory limit**,
  whatever the account's quotas: a pod without one may take all of its node's
  memory, and Java takes 95% of it. The same goes for removing a server's
  limit.

### Fixed

- **Changing the backup target no longer leaves backups that cannot be
  restored listed as if they could.** The panel did not record where each
  backup went, so after a change the old ones were still offered for restore,
  and a restore failed with "the backup repository" and nothing else. Backups
  now record their target: those made to a previous one are marked in the
  Backups tab, their restore is refused with the reason, and deleting one
  removes it at once instead of waiting on a snapshot deletion that could only
  fail. Backups made before this version are marked the next time the target
  changes. Failed backups and restores also say what went wrong, where they
  used to quote only the repository's (hidden) location.
- **Saving a backup target the panel cannot reach now says so.** It is still
  saved, since the backup jobs may reach what the panel cannot, but with a
  warning where it used to be accepted without a word.
- **When a node goes down, the panel says so.** A server whose data is on it
  used to show "Hibernated", or "Starting" forever once started, without a
  message; its status now names the node that is not responding, or says that
  no node can take the server and why. The file manager answers at once with
  that reason, where it waited two minutes and then blamed a restore.

### Security

- **Failed sign-ins no longer lock an account's owner out.** Ten wrong
  passwords for an account, from anywhere, blocked it for everyone for fifteen
  minutes, so anyone who could reach the panel could keep its administrator
  out with forty requests an hour. A browser that has signed in to an account
  now keeps a cookie that gives it a count of its own; the others still share
  the account's, so guessing from many addresses gets no further than before.
  Signing in also stopped clearing the per-address count, which let someone
  with an account of their own reset it between two volleys at others, and an
  IPv6 address now counts with the rest of its /64.
- **A suspended server is frozen for its owner and subusers.** Suspension
  refused them power and files and nothing else, so the owner of a server
  suspended for abuse could still delete it, data and all, before anyone
  looked into it, rename or reinstall it, or queue backup after backup until
  retention had pushed out every snapshot from before; its scheduled backups
  did the same on their own. Until an administrator lifts the suspension, they
  can now only look at it: everything else answers 409, its schedules do not
  run, and the panel shows why. File requests on a suspended server answer
  409 rather than 403, like the rest.
- **Requiring a second factor no longer catches the superadmin who requires
  it.** One without a second factor who set the policy was left the
  enrolment page and nothing else, their API keys refused, turning it back off
  included, with no warning. The panel now refuses a policy that covers you
  until you have a second factor, and the Two-factor policy card says how
  many accounts a policy would hold to enrolment and how many of their API
  keys it would stop, before you save.
- **The chart keeps a PostgreSQL DSN out of the Deployment.** The DSN,
  database password included, was a plain environment value of the
  Deployment, readable by whoever may read Deployments in the namespace, and
  it had to sit in the values in clear. The chart now puts it in a Secret, and
  `db.existingSecret` / `db.existingSecretKey` read it from one of your own
  (SOPS, External Secrets). A PostgreSQL install's pods restart once on the
  upgrade; a SQLite DSN is only a path and stays where it was.
- **The audit log records the changes it missed.** Pointing the backups at
  another target, which decides where every server's data goes, editing a
  schedule, changing an account's email or password (a reset through the
  emailed link included), and revoking an API or SSH key left no entry. They
  now do: a backup target's entry names where it points and which secrets
  changed, never their values, and a schedule's entries show the commands it
  sends.

## [0.5.1] - 2026-09-30

The fixes from a full test of 0.5.0 on three clusters: eight serious bugs and a
quota an account could exceed. Pelican eggs such as Rust and Factorio run, and
so does Valheim; a cluster registered with the setup manifest runs servers;
backups say when they fail; and upgrading Quetzal no longer restarts the game
servers, from this upgrade on.

**Upgrading from 0.5.0** — two kinds of pods restart once, and nothing else:

- Servers created from an imported egg, to get the `/etc/passwd` that names
  their user. See *Fixed*.
- The activators of servers published with the player's address kept (the
  default), to move next to their game. A proxy-mode activator drops the
  players connected through it. See *Fixed*.

### Fixed

- **Pelican eggs that write `{{server.environment.X}}` work.** Quetzal left
  that placeholder as written, so the game read it literally: Rust failed to
  load the level `{{server.environment.LEVEL}}` and Factorio's
  `server-settings.json` no longer parsed. About fifty eggs use it, ARK, DayZ
  and Satisfactory among them.
- **Games that look up the user they run as no longer crash.** Quetzal runs
  imported eggs as uid 988, which their images don't know, so a game that
  asked found nothing: Valheim segfaulted at every start. The game container
  now gets an `/etc/passwd` and an `/etc/group` that call it `container`, with
  the server's directory as its home, as Wings does. Servers from imported
  eggs restart once after the upgrade to get them.
- **Servers run on a cluster registered with the setup manifest.** In each
  namespace it created there, the control plane bound the role of its own
  chart, which the account the manifest creates is not allowed to hand out:
  the namespace appeared and nothing else, with nothing said in the panel, and
  a transfer to that cluster stalled the same way. It now binds the role the
  manifest creates. A cluster registered with an administrator's kubeconfig
  was not affected.
- **Upgrading Quetzal no longer restarts the game servers.** Their pods run
  helpers out of the panel's image (the config file renderer, the SFTP server,
  the proxy activator), whose tag changes with each release, and the new tag
  alone made Kubernetes replace every such pod at once, kicking the players. A
  running pod now keeps its helper image until it restarts for another reason,
  as [the upgrade guide](docs/UPGRADE.md) always said it would. This holds
  from this upgrade on.
- **A game that fails at every start shows as crashed at once.** Kubernetes
  1.35 reports such a container as terminated through the first back-offs and
  calls it CrashLoopBackOff only when they reach minutes; Quetzal waited for
  that word, and the server stayed "Starting" for five minutes. The message
  now says how the game ended ("the game exited with code 1", or that it ran
  out of memory) instead of repeating the kubelet's back-off line.
- **Backups and restores notify when they end.** Only the request that
  started one was an event, so a scheduled backup failing every night, on an
  expired key or a full bucket, was told to no one. Four events now say how
  they ended, with the error when there is one: `backup.succeeded`,
  `backup.failed`, `restore.succeeded` and `restore.failed`. They reach the
  channels that take every event and the server's activity log; a channel
  that filters its events needs them ticked.
- **Writes at the same moment no longer fail on SQLite.** Creating 15
  servers at once through the API failed 7 of them with "database is locked
  (SQLITE_BUSY)", despite the busy timeout: a transaction that reads before
  it writes could not wait for the lock. Write transactions now take it when
  they begin, and wait for it. A failed node-port allocation is a 409 only
  when the range is used up, and a 500 answer is written to the log.
- **On a cluster of several nodes, a server's node-port address answers.**
  A Service that keeps the player's address, the default, answers only on the
  nodes running its pods, and the panel showed the first node's address
  wherever the pods ran. It now shows the address of the node the server runs
  on. The activator of a sleeping server moves to that node too, so the
  address still answers once the game wakes; activators restart once after
  the upgrade to get there. A hostname set in the settings is still shown as
  it is.

### Security

- **A quota holds when requests arrive together.** An account could exceed
  the quotas an administrator set by sending its requests at once: five
  servers created together by an account allowed one made two, since each
  request checked the quota before any had created its server. The check and
  the creation are now one transaction, and so are the check and the change
  when a server's memory or CPU is raised.

## [0.5.0] - 2026-09-28

Quetzal on ARM, and a chart you can install from a registry. The image now
exists for arm64, so the panel runs on ARM clusters, though many games still
need amd64 nodes. The Helm chart is published to GHCR and signed, with a README
and a values schema, and the panel's pod meets the Pod Security Standards'
`restricted` level. One fix matters: uninstalling the chart deleted the
panel's database and its encryption key.

**Upgrading from 0.4.0** — three things behave differently:

- The chart refuses values it doesn't know. A key left over from an older
  chart, or mistyped, used to be ignored; the upgrade now stops and names it.
  Remove it and run the upgrade again. See *Added*.
- The chart needs Kubernetes 1.30 or later; Helm refuses an older cluster. See
  *Changed*.
- The upgrade replaces the panel's pod, whose security settings change. Game
  servers keep running. Upgrade from the registry:
  `helm upgrade quetzal oci://ghcr.io/lolozini/charts/quetzal --version 0.5.0 --reuse-values`
  (the release's chart file works too).

### Added

- **arm64 images.** The Quetzal image now exists for arm64 as well as amd64,
  so the panel runs on ARM clusters. Games are another matter: many only exist
  for amd64, Valheim and everything installed through SteamCMD among them, and
  on a cluster with both kinds of nodes Quetzal doesn't yet keep them off the
  arm64 ones. See *CPU architectures* in the
  [install guide](docs/INSTALL.md#cpu-architectures).
- **The Helm chart is in a registry.** Install it with `helm install quetzal
  oci://ghcr.io/lolozini/charts/quetzal`, signed like the images; the file
  attached to each release stays. The chart also gets a README, shown by
  `helm show readme`, and a values schema that refuses unknown keys, so a typo
  fails the install instead of being ignored.

### Changed

- **The panel's pod meets the Pod Security Standards' `restricted` level.**
  Its containers run with a read-only root filesystem, no privilege escalation
  and no capabilities, under the runtime's default seccomp profile, so it
  installs in a namespace that enforces `restricted`.
- **The chart needs Kubernetes 1.30 or later** and says so: Helm refuses an
  older cluster instead of failing later on the admission policy.

### Fixed

- **`helm uninstall` deleted the panel's database** along with its encryption
  key, when the chart had created them. Both are now kept, and installing again
  under the same release name picks them up.

## [0.4.0] - 2026-09-25

Quetzal gets its own look, and its releases can be verified. The panel wears
the new visual identity: a logo, a favicon, warm dark colours and its own
fonts. Images and the release chart are signed, with an SBOM. One security fix
matters: a line break in a startup variable could add any key to a server's
configuration files, `online-mode=false` for instance.

**Upgrading from 0.3.1** — one thing to check:

- A config file that already grew duplicate lines at each start keeps them;
  Quetzal stops adding more, but delete the extras once. See *Fixed*.

### Added

- **Signed images.** The images on GHCR are signed with cosign, keylessly, by
  the workflow that builds them, and carry an SBOM and their build provenance.
  The Helm chart attached to a release is signed too. The
  [security policy](SECURITY.md) shows how to verify them.

### Changed

- **Quetzal has a visual identity.** The panel shows the new logo, a
  Quetzalcoatlus in flight, in the top bar and on the sign-in page, and the
  browser tab shows a favicon. The colours are warm darks with a rust accent,
  and the fonts are Bricolage Grotesque, Instrument Sans and JetBrains Mono.
  The fonts ship with the panel, so it loads nothing from a CDN. Each server
  state has its own colour and a dot:

  - Running: green;
  - Hibernated: blue;
  - Starting, Installing and Stopping: amber;
  - Crashed: red.

  A destructive button is now red and outlined. The logo files and how to use
  them are in the [brand guide](docs/brand/README.md).

### Fixed

- **Some configuration files grew at every start.** In an INI file, a key
  outside any section was added at the end of the file, inside the last
  section, where the next start didn't find it and added it again. A key with
  spaces around it did the same in a `properties` file. Missing keys now go
  into their own section, keys are matched the way they are written, and a
  stray carriage return no longer turns into a Windows line ending.
- **Network and disk figures can no longer go negative.** They are read from a
  command's output inside the game container; a negative or overflowing number
  there is now ignored.

### Security

- **A startup variable could add lines to a server's configuration files.** A
  line break in a variable's value was written as is into the `properties`,
  `ini` and `file` configurations an egg manages. Whoever could edit a server's
  variables could then set any key of those files, `online-mode=false` for
  instance, without access to the files. A value is now kept on its line.

## [0.3.1] - 2026-09-25

A security fix: the guard on outbound requests let a user-supplied URL reach
the carrier-grade NAT range, where Tailscale nodes and, on some clusters, pods
live.

**Upgrading from 0.3.0** — one thing behaves differently:

- A webhook, an egg URL or a Pterodactyl panel on a 100.64.0.0/10 address (a
  Tailscale node, for instance) is now refused, as private addresses already
  were. See *Security*.

### Security

- The outbound guard (webhooks, eggs by URL, Pterodactyl imports) also
  refuses the special-use IPv4 ranges Go's `IsPrivate` leaves out, chiefly
  100.64.0.0/10: carrier-grade NAT, which is also where Tailscale puts its
  nodes and where some clusters (EKS with custom networking, among others) put
  their pods, so a user-supplied URL could reach them. Also 0.0.0.0/8,
  192.0.0.0/24, 198.18.0.0/15 and 240.0.0.0/4, and NAT64 addresses: the
  well-known prefix is judged by the IPv4 address it embeds, the local-use one
  is refused.

## [0.3.0] - 2026-09-25

Migrating from Pterodactyl, and a server that behaves the way players expect.
New: importing a server from a Pterodactyl panel in one step, Pelican's YAML
eggs, the `xml` config parser, a complete file manager, renaming a server and
console history. A server is now Running when the game says it is up, not when
its container starts, and a sleeping Minecraft server wakes only for a player,
not for every port scanner on the internet. Among the fixes, one that matters:
restoring a backup taken before a reinstall wiped the restored data.

**Upgrading from 0.2.0** — four things behave differently:

- A server shows **Starting** until its game prints its template's done line;
  it used to show Running as soon as its container started. Servers already
  running when you upgrade stay Running. See *Changed*.
- A sleeping **Minecraft** server wakes only for a player joining; server-list
  pings show it asleep, and connections to its other ports wake nothing. See
  *Changed*.
- On an install that runs the `latest` image, the upgrade restarts the servers
  that render config files, once. An install on a version tag is not affected.
  See *Changed*.
- Server names are limited to 190 characters, on one line.

### Added

- **Import a server from Pterodactyl.** In *New server*, *Import from
  Pterodactyl* takes the address of the server's page and a client API key
  (`ptlc_…`), fills the form from the server (template matched by the egg's
  name, image, variables, memory/CPU/disk limits, allocations as ports) and
  lists what does not carry over. On creation, the panel backs the server up —
  or compresses its files when it has no backup slot — and the archive streams
  into the new volume; the server is then marked installed, so the egg's
  install does not run over the files, and the panel-side archive is deleted.
  Progress and failures show on the server's page, with a retry. The key is
  never stored. API: `POST /api/import/pterodactyl/inspect`, a `pterodactyl`
  source on `POST /api/servers`, and `POST /api/servers/{id}/import/pterodactyl`.
- Eggs in **Pelican's YAML format** (`PLCN_v3`, the `egg-*.yaml` files of the
  pelican-eggs repositories) are read, by paste or by URL, alongside the
  Pterodactyl JSON.
- The **`xml` parser for `config.files`**, as Wings implements it: a dotted key
  is an element path from the root, missing elements are created, a
  `[name='value']` value sets an attribute, and `*` matches every element.
  Space Engineers, Trackmania 2020 and two GTA multiplayer eggs use it; their
  config files were left untouched before. One difference from Wings: a key
  naming another root element changes nothing, instead of adding empty
  elements to the file.
- **File manager**: copy a file or folder (`name copy.ext`, as on
  Pterodactyl), extract an archive already on the server (.zip, .tar,
  .tar.gz, .tar.bz2, .tar.xz), select several entries to delete, move or
  archive them at once, and a sortable modification-date column.
- A CPU limit field in the create form.
- `docs/MIGRATING.md`: bringing eggs (from your panel, or from pelican-eggs by
  URL) and servers over from Pterodactyl.
- **Rename a server** from its Settings tab (`name` on
  `PATCH /api/servers/{id}`). Only the displayed name changes; the slug, the
  Kubernetes objects and the address stay put. Names are trimmed, one line,
  and at most 190 characters, at creation too.
- **Console history**: the up and down arrows bring back the commands sent
  earlier, per server, as in Pterodactyl (kept in the browser, 50 at most).

### Changed

- **A server was reported Running before it was up.** Running meant the
  container had started, while a Minecraft world, say, still had a minute or
  two to load: players who connected were refused, and "is up and running" was
  announced early. The server now stays Starting until the console shows one of
  its template's done lines (the egg's `config.startup.done`), as Pterodactyl
  waits for, and the page says which line it is waiting for. A done line that
  never shows within 30 minutes no longer holds the server back: it is reported
  Running, with a message that the line may be out of date. Eggs that list
  several done lines are now read too (they used to be dropped), and templates
  store them as `done`; the older single `doneRegex` is still read.
- **Port scanners no longer wake a sleeping Minecraft server.** Wake-on-connect
  woke on any TCP connection, so every scanner on the internet could wake a
  public server, and each wake bought it a full idle window: it spent its
  nights cycling awake for nobody. The activator now reads the client's
  Minecraft handshake. A server-list ping is answered by the activator, with
  the server shown as asleep; a player joining wakes it and is asked to
  reconnect in a minute; anything that is not a Minecraft client, and any
  connection to another port (RCON, query), wakes nothing. In proxy mode, only
  a player's traffic now counts as activity, so a scanner polling the server
  list no longer keeps it awake either. This applies to templates with the
  `eula` egg feature, which Minecraft Java servers and proxies carry; a
  template's new `wakeProtocol` (`minecraft` or `any`) overrides it. Other
  games still wake on any connection.
- The helper containers that run the Quetzal image (the wake-on-connect
  activator, the config render and the SFTP copy) follow Kubernetes' default
  pull policy for that image instead of always `IfNotPresent`. On an install
  that runs `latest`, a node without a Quetzal pod of its own kept the first
  `latest` it had pulled, and ran old helpers against a newer controller; they
  are now pulled like the control plane's own pods. A version tag is pulled
  once per node, as before. On a `latest` install, the upgrade restarts the
  servers that render config files, once.

### Fixed

- The first reconcile of a new server no longer logs "cannot patch resource
  resourcequotas": the access the control plane grants itself in the new
  namespace is now given a moment to take effect before it is used.
- **Restoring a backup could wipe the restored data.** A reinstall's "wipe the
  data" flag was never cleared, and a restore brought back the install marker
  of the snapshot's day: restoring a backup taken before the last reinstall
  made the next start run the install again, wiping the volume first. A
  restore now marks the data installed (as on Pterodactyl), and the wipe is
  retired once its reinstall has run.
- **Server owners could not back up from the panel.** The Backups tab read the
  backup target, which only settings admins may see, so for everyone else it
  showed an error and kept "Backup now" disabled. Every user now learns
  whether backups are configured; the target stays admin-only.
- Hibernation now sends the template's stop command before scaling a server
  to zero, as stopping does. The game used to get only SIGTERM, which a
  startup wrapped in a shell does not pass on, so it went down without saving.
- The wake-on-connect activator no longer stays up in front of a server that
  was stopped or suspended while asleep.
- Uploading a file through the file manager no longer fails after a minute:
  the write lasted as long as the upload, and was cut at 60 seconds. Deleting
  a large folder had the same limit.
- A backup or restore Job gets the retry it was built with: the operation was
  declared failed, and its Job deleted, on the first failed attempt.
- `PATCH /api/users/{uid}` changes only the fields it is sent. A request that
  only reset a password also set the account's quotas to 0 (unlimited) and
  demoted an administrator; nothing is written any more when one field is
  refused.
- A scheduled start or restart is skipped, and a restore refused, while the
  server is being transferred to another cluster; either could stall the
  transfer for good.
- Re-importing an egg no longer resets its template's creation date.
- The console no longer leaks a goroutine when a line was typed while no
  container was attached.
- The SQLite database gets a busy timeout by default, as the Helm chart
  already set; two processes share it, and a concurrent write failed at once
  with "database is locked".
- The drop-mode activator closes a connection before calling wake, instead of
  holding its accept loop for the length of the call.
- An imported egg's default image is the first one it lists, as in
  Pterodactyl. It used to be whichever came first out of a Go map — a
  different one from one import to the next. Re-import an egg to fix a
  template imported before.
- `{{server.allocations.default.port}}`, `{{server.allocations.default.ip}}`
  and `{{server.build.memory_limit}}` — the paths Wings actually resolves, which
  Pelican's eggs use — are translated like their `server.build.*` aliases
  instead of being written out literally.

### Security

- `golang.org/x/oauth2` 0.27.0 and `filippo.io/edwards25519` 1.1.1, which ship
  in the image, for the advisories GitHub raised on them; govulncheck finds no
  call to either from Quetzal.
- The web toolchain moves to vite 6 (esbuild 0.25), with its transitive
  dependencies at their fixed releases: eight advisories on the dev server and
  the build, none of which reached the image. `npm audit` reports none.

## [0.2.0] - 2026-09-23

Mostly hardening: a security review of the whole panel, the install path made
honest about failure, notifications that retry and show when they cannot, and a
release pipeline whose chart installs. New: changing a server's template or
image on reinstall, a required second factor, server search, schedule time
zones, paged logs and log retention.

**Upgrading from 0.1.0** — four things behave differently:

- `/metrics` is no longer served on the panel's port: scrape container port 9091
  (apiserver) and 9090 (controller) instead. See *Changed*.
- Email set to STARTTLS (the default) now requires it: a relay that does not
  offer STARTTLS needs **None (cleartext)** chosen explicitly. See *Changed*.
- Imported eggs now keep their data under `/home/container`, as on Pterodactyl;
  re-import an egg to pick up the new path. See *Fixed*.
- Built-in templates are no longer seeded; ones already in the database stay.
  See *Removed*.

### Added

- **Change a server's template or image when reinstalling.** A reinstall can now
  move the server to another template — another egg for the same game (Paper to
  Fabric, keeping the world) or another game — and to another of its images.
  Ports, resources, storage and, unless wiped, files are kept. Variables the new
  template also declares keep their value, secrets included, unless it fixes them
  or the value is not one of its options; those take the new defaults and are
  listed back. Only the server's owner or an administrator can change the
  template; a subuser with **settings** can still reinstall. `template`, `image`
  and `env` on `POST /api/servers/{id}/reinstall`.
- **Public hostname for server endpoints.** Admin → Network takes a DNS name that
  is published to players instead of the raw node IP, for both a server's game
  endpoints and its SFTP connection string; the page shows the detected node
  address as a hint for the record to create. A cluster can override it from
  Admin → Clusters → Hostname, since each cluster fronts its own nodes and one
  global name would advertise the wrong address for servers elsewhere. Blank
  falls back to the node address, so existing installs are unaffected.
  `GET`/`PUT /api/network-settings`, and `endpointHost` on a cluster.
- **TCP and UDP on the same port number.** A port can now serve both protocols on
  one number (Minecraft Java's game + query on 25565, a Source game + RCON on
  27015): pick **TCP / UDP** in the ports editor and both are created, sharing a
  single external node port. Previously this was silently broken — both ports
  received the same generated name, which Kubernetes rejects, leaving the server
  with no networking.
- **Crashes, OOM kills and restarts are recorded.** A server that keeps dying and
  coming back — classically an out-of-memory loop — used to churn with no trace
  in the panel. The controller now reads each container's last termination and
  emits `server.oomkilled` (or `server.restarted`, carrying the exit code) on
  every newly observed restart. These show in the activity log and can fan out to
  notification channels like the existing crash event.
- **The server activity log now shows controller events too.** It reads the event
  feed rather than the audit log, so crashes, OOM kills, restarts and readiness
  appear on the same timeline as user actions (controller entries are attributed
  to `system`). The admin-wide activity log gained a **Server** column, so an
  action like `server.power start` finally says which server it hit.
- **Restart hint on the server page**: while a section has an unsaved edit that
  will bounce the server (Variables, Resource limits, Ports), a small `↻`
  "restarts the server" marker appears next to its heading — so you know before
  saving. It shows only when there's a pending change; controls that apply
  without a restart (Exposure, Hibernation) show nothing.
- **Editable ports after creation**: a server's per-server ports (number,
  TCP/UDP, primary) can now be changed from **Settings → Ports**, not just at
  creation. Saving reallocates pool node ports as needed (unchanged ports keep
  their allocation, removed ones are freed, the SFTP port is untouched) and
  restarts the server on the next reconcile. `PATCH /api/servers/{id}` gained a
  `ports` field; the create form and the settings editor now share one ports
  component.
- **Searchable template picker on the create-server form**: the template
  dropdown is now a combobox — open it and start typing to filter by name (no
  separate search field). Imported eggs are named after their catalog path (e.g.
  `minecraft/java/paper`), so a search matches by game, category or variant.
- **Port suggestions for imported eggs**: the create form now pre-fills the
  per-server ports editor from the template's port-like variables (`QUERY_PORT`,
  `RCON_PORT`, `STEAM_PORT`…, detected by name + a valid numeric default), so a
  server imported from a Pterodactyl egg starts with its extra ports already
  filled in instead of a blank row. The editor gained a per-row "primary"
  selector to pick the port players connect to (the main game port is usually the
  allocation, not a variable). Exposed as `Template.suggestedPorts` in the API.
- **Per-server ports** can be defined at creation for templates that declare none
  (imported Pterodactyl eggs allocate ports per server, not in the egg): a small
  ports editor (number + TCP/UDP, first is primary), and the network-exposure
  selector appears once a port is defined.
- **Minecraft EULA acceptance** for templates that declare the `eula` egg
  feature: an "I accept the Minecraft EULA" toggle on create/settings; when
  accepted, the controller renders `eula.txt=true` into the data volume at
  startup (and writes nothing otherwise, so the server keeps asking). Mirrors
  Pterodactyl's `eula` feature without modifying the imported egg.
- **The logs page back through their history.** The audit log and the event feed
  answered with their newest entries and nothing else — 200 for the panel-wide
  log — so an admin looking up what happened last week simply could not. Both now
  take a `before=<id>` cursor and report the total in `X-Total-Count`, and the
  activity views grow a **Load older** button that walks to the beginning.
- **Two-factor authentication can be required panel-wide.** Admin → **Two-factor
  policy** sets it to off, administrators only, or everyone; superadmin-only to
  change, because it decides who gets in. Turning it on locks nobody out,
  including the superadmin who turned it on: an account the policy covers keeps
  its session but reaches only `/api/me`, enrolment and logout until it has a
  second factor, and the panel shows the enrolment page instead of a wall of
  refusals. `GET`/`PUT /api/security-settings`.
- **Search the server list.** The dashboard fetched every server on the panel on
  every load, with nothing to narrow it. `GET /api/servers?q=` filters on slug
  and display name in the database, and the list has a search box. A `%` or `_`
  in the query is literal, so searching for "100%" finds the one server rather
  than all of them.
- **Schedules have a time zone.** A cron was read in whatever zone the control
  plane runs in — UTC in a container — so "restart at 4am" fired at 6am local in
  a European summer, and the only lever was a panel-wide `TZ` in `extraEnv`. A
  schedule now carries an IANA zone (`timezone`, e.g. `Europe/Paris`), validated
  on save, and the form prefills it from the browser so the common case is right
  without anyone knowing the panel runs in UTC. Existing schedules keep the old
  behaviour (an empty zone still means the control plane's own). The binaries
  embed the zone database: the runtime image is distroless and carries no
  `/usr/share/zoneinfo`, so a named zone would otherwise fail to load in
  production while working on every developer machine.
- **Log retention.** `retention.eventDays` (default 30) prunes the event outbox,
  which is written on every power action, crash and restart, read by the
  dispatcher through a cursor, and was never emptied — so it only grew, for the
  life of the install. Pruning stops dead at the dispatcher's position: an event
  that has not gone out yet is never dropped, so no notification is lost to it.
  `retention.auditDays` defaults to **0, keep everything** — an audit log is an
  accountability record, and deleting one because a default said so is not a
  decision to make for an operator.
- **`replicaCount` in the chart.** It was pinned at 1 with no way to change it,
  while the code had already done the work that makes more than one safe:
  leader election in the controller, rate-limit counters in the database. The
  chart refuses `replicaCount > 1` on SQLite, and with a ReadWriteOnce claim
  still enabled, rather than letting either be discovered in production — two
  pods cannot share one SQLite file. Above one replica the rollout strategy
  becomes `RollingUpdate` instead of taking the panel down.
- **Notification delivery is retried, and a failing channel shows in the panel.**
  A delivery used to be dropped on its first error, with a log line as the only
  trace. It now gets three attempts with backoff, honouring `Retry-After`; a
  refusal that will not change (a 404 from a deleted webhook) is not retried. A
  channel that still fails is marked in Notifications with the number of events
  it has missed in a row and the reason (`failureStreak`, `lastError`,
  `lastErrorAt`, `lastDeliveryAt` in the API); a successful test clears it.

### Changed

- **Node ports are allocated at random within the pool.** Allocation used to take
  the lowest free port, so a server's ports followed its neighbours' and were
  trivially guessable from the outside. The range is now scanned from a random
  start. Ports are also keyed by port *number* rather than by name, which is what
  lets a TCP/UDP pair share one external port and means adding or removing a
  protocol no longer moves the address players connect to. Existing allocations
  are kept (and adopted if they were held under an older key).
- **Notifications carry the server's real name.** Discord messages are now embeds
  with the same fields as the activity log (event, server, user, time, plus a
  colour keyed to severity); webhooks gained `serverName`/`serverSlug` alongside
  `serverId`; email gained a structured Server/Event/User/Time block and the
  server name in the subject. The name shown is the display name, not the slug.
- **The current page survives a reload.** The view is encoded in the URL fragment
  (`#/servers/<id>`, `#/admin`, …), so refreshing a server or admin page stays
  put instead of dropping back to the server list, and the browser's back and
  forward buttons work.
- **The console reconnects on its own.** Starting a stopped server used to leave
  the console dead until the page was reloaded; it now re-establishes as soon as
  the server is live again, backing off between attempts so a crash-looping
  server doesn't hammer the API. The SFTP panel likewise polls for its port until
  it is provisioned, replacing the manual Refresh button.
- **The Resources panel is quiet when a server is stopped.** It no longer polls
  (or prints `no pod found …`) for a server with no pod, showing a neutral
  placeholder instead; crash detail belongs to the activity log.
- **Deleting a server now always removes its data volume.** The keep-or-destroy
  prompt is gone: deleting a server tears down its namespace, cascading the PVC
  and the underlying volume/data, so nothing is left orphaned. **Breaking:** data
  is always deleted — back it up first if you need to keep it. Removed the
  `retainOnDelete` storage flag and the `?keepData=` delete parameter.
- **Server names are no longer required to be unique.** A server's display name
  is now a free label — duplicates are allowed, matching Pterodactyl/Pelican.
  Its stable identity is the slug: a readable prefix from the name plus a short
  random suffix (e.g. `survival-a1b2`), generated once at creation and never
  changed. This fully decouples the per-server namespace and Kubernetes
  resources from the name (so naming can never cause a resource collision, and
  renaming becomes possible). Previously a second server with the same name — or
  a name that slugified identically — was rejected with a 409. Existing servers
  keep their slugs.
- **Friendlier resource readouts** on the server page: CPU is shown in **cores**
  (e.g. `0.01 cores`) instead of raw millicores (`12m`), and both CPU and Memory
  show **used / limit with a percentage** when the pod has a limit set (e.g.
  `1.13 GiB / 4.00 GiB (28%)`), matching the disk bar.
- **Long sections collapse to keep pages short.** The activity log (admin and
  per-server) and the admin **Eggs / templates** section are now collapsible and
  start collapsed, showing a count in the header — so a busy audit trail or a
  large template list no longer stretches the page. Click the header to expand.
- **Storage is now always a PVC.** Removed the user-selectable `hostPath` storage
  type: it let a tenant mount arbitrary node paths (a host-escape vector for the
  untrusted code game pods run, and disallowed by the baseline/restricted Pod
  Security Standards) and had no scheduling affinity, which broke rescheduling and
  cross-cluster transfer. Single-node setups use a local provisioner (e.g.
  local-path) as the storageClass. **Breaking:** servers created with `hostPath`
  storage must be recreated.
- **storageClass is admin-controlled per cluster**, chosen from a dropdown of the
  cluster's actual storage classes (Admin → Clusters), instead of a free-text
  field at server creation. New servers inherit the target cluster's default.
- **Files and SFTP now work whether the server is running or stopped**, with no
  startup latency. A small always-on **data-manager pod** mounts the data volume
  permanently and hosts file operations and the SFTP server; the game pod is
  co-located with it (podAffinity) so both share the ReadWriteOnce volume on one
  node. Replaces the previous on-demand maintenance pod (which only ran while
  stopped) and the SFTP sidecar (which only ran while running). During a restore
  the data-manager is scaled down so the restore gets exclusive volume access.
- **Metrics moved off the panel's port.** The apiserver serves `/metrics` on
  container port 9091 (`api-metrics`), the controller on 9090; the Service
  publishes neither. With the chart's Ingress, `/metrics` used to be readable by
  anyone at the panel's URL, and each request scanned the servers table. **A
  scraper pointed at `https://<panel>/metrics` must be pointed at the pod** — it
  now gets the panel's HTML instead.
- **Email in STARTTLS mode requires STARTTLS.** The mode the form calls STARTTLS
  (the default, also used when unset) went ahead in cleartext when the server did
  not offer it — which is what an attacker on the path arranges by deleting the
  offer. Passwords were never sent that way (Go refuses), but the messages were,
  and they include password reset links. **A relay that offers no STARTTLS now
  refuses to send** until **None (cleartext)** is chosen for it explicitly; the
  test-email button shows the error.

### Removed

- **The egg catalog is gone.** The catalog manifest URL, its browse/install list
  and the two endpoints behind it never earned their keep next to pasting an egg
  or importing it by URL, both of which remain. `GET`/`PUT /api/egg-catalog` and
  the stored catalog setting were removed with it.
- **Built-in templates are no longer seeded into the database.** Fresh instances
  start with an empty template list; add templates by importing Pterodactyl/Pelican
  eggs (the intended workflow). The former built-ins (Paper, CurseForge, Valheim,
  a generic process) were redundant with egg import and cluttered the picker. They
  remain in the tree only as test fixtures. Existing seeded templates can be
  deleted from Admin → Templates and will not come back on restart.

### Fixed

- **The v0.1.0 chart installed an image that was never published.** Its default
  image tag was `v0.1.0`, but the release pushed only `0.1.0`, `0.1` and `latest`,
  so installing the chart as published ended in `ImagePullBackOff` — as did the
  `--set image.tag=vX.Y.Z` the docs recommend. Releases now push the tag as
  written too, and refuse to publish a chart whose default image they did not
  push. With 0.1.0, set `image.tag=0.1.0`.
- **Every namespaced object in the chart names its namespace.** The Deployment,
  Service, ServiceAccount, PVC, Ingress and generated Secret left it to Helm, so
  `helm template | kubectl apply` or `kubectl diff` put them in the kubectl
  context's default namespace instead of the release's. `helm install` itself
  was unaffected.
- **Wings placeholders mean the same thing in the startup command as in
  `config.files`.** `{{server.build.env.X}}`, `{{env.X}}`,
  `{{server.build.default.port}}` and `{{server.build.memory}}` reached the game
  as literal text when used in a startup command; `config.files` had always
  translated them. (None of the commonly used eggs puts them in a startup.)
- **A scheduled `start` never woke a hibernated server.** The schedule reported
  success while the server stayed scaled to zero: the task set the desired state
  to Running but left the hibernation flag set, and a server counts zero replicas
  while it is hibernated. Starting also rearms the idle timer now, so a server
  brought up by a cron is no longer put straight back to sleep on the next
  hibernation tick because its last-activity timestamp was hours old. Both start
  paths (power action and scheduled task) share one store operation, so they
  cannot drift apart again.
- **Deleting a backup left the data in the bucket.** Only the database row was
  removed; the restic snapshot stayed in S3 for good, so storage kept growing and
  "deleted" data survived. A succeeded backup now enters a **Deleting** phase
  while the controller forgets its snapshot from the repository, and the record
  disappears only once that succeeds — a failure puts the row back with the
  reason instead of quietly stranding the data. Snapshots belonging to a *deleted
  server* are still retained: its namespace, and with it any Job that could prune
  them, is gone by then; remove that server's repository prefix from the bucket
  by hand if you need the space.
- **Deleting an in-flight backup or restore could corrupt the data volume.**
  Nothing stopped the record from being dropped mid-operation, and the
  reconciler keeps the data manager scaled down only *while a restore row
  exists* — so deleting one brought the data manager back up to write to the
  same ReadWriteOnce volume the restic Job was restoring into, and orphaned the
  Job (cleanup is driven from the row). An operation that is Pending or Running
  now refuses deletion with `409`, and the UI disables the button.
- **Symlinks could reach outside a server's data directory.** Paths were confined
  as text, which stops `..` but not a symbolic link — and one can appear in the
  volume without going through the panel at all, since archive extraction
  recreates the links an archive contains. A subuser holding only the *files*
  permission could plant one and then read the container's filesystem, including
  the server's SFTP host key. Both the file API and the SFTP server now resolve
  the path before using it and refuse anything landing outside the data
  directory; a link is still deletable and renameable, so a planted one can be
  cleaned up.
- **A backup that succeeded could be reported as failed.** A finished Job that
  the controller had not yet read was treated as vanished; its retention is now
  long enough for a restarted or non-leader controller to see the real outcome.
  A retried Job also no longer reports the wrong attempt's size or error.
- **Two control planes on one cluster deleted each other's servers.** Orphan
  collection means "no server row in *my* database", and it selected namespaces
  by a fixed `managed-by: quetzal` label with no notion of which instance owned
  them — so a second Quetzal pointed at the same cluster (a staging panel beside
  production, say) tore down the other's servers within one resync, in both
  directions. The same held for managed-database namespaces, where it destroys
  the data rather than a recreatable server. Each control plane now has a stable
  instance id derived from its database, stamps it on the namespaces it creates
  (`quetzal.dev/instance`), and only ever reclaims namespaces that are its own
  or unlabelled; with no id resolvable, collection does nothing rather than
  guess. Existing namespaces are adopted on the next reconcile, so nothing
  changes for a single-instance install.
- **Deleting a user did not revoke their SFTP access.** Their sessions, API keys
  and server grants were removed, but their SSH keys were not — and a server's
  `authorized_keys` is built from its owner id, which outlives the account. A
  deleted user therefore kept SFTP access to every server they owned. Keys (and
  any pending password-reset token) are now removed with the account, and the
  authorized-keys query ignores keys belonging to accounts that no longer exist.
- **An unreadable config file was silently replaced.** The config renderer runs
  on every start and rewrites each managed file from what it read; a read error
  was reported as "empty", so a config it merely could not open came back holding
  nothing but the managed keys. It now fails the render and leaves the file
  alone — only a genuinely absent file is created from scratch.
- **Power and transfer messages were always in English.** The notice shown after
  every start/stop/restart/kill, and the transfer confirmation, bypassed the
  translation layer. `server.stopped` and `server.transfer` can also be picked
  as notification events now — the list offered "came up" without "went down".
- **The web client failed on successful empty responses.** Only `204` was treated
  as bodyless, so any other success without a body (a `202` acknowledgement) blew
  up parsing JSON and surfaced a completed action as an error.
- **A lost file write could replace your file with an empty one.** Uploads
  streamed into `cat > file` in the pod, so a stream that delivered nothing —
  which happens against a container that has only just started — left the file
  truncated to zero while the API still answered success. Writes now spool beside
  the target and move into place only once the whole payload has arrived,
  verified against `Content-Length`; a lost or short write fails loudly and
  leaves the existing file untouched, and small bodies are retried once.
- **Mail headers could be injected through a server name.** A display name
  containing a line break was interpolated straight into the notification email's
  Subject, letting it add headers (`Bcc:`) or author the message body. Header
  values are now stripped of line breaks, and the subject is RFC 2047 encoded —
  which also fixes accented names and the em dash producing a raw 8-bit header
  that strict mail servers reject.
- **The graceful stop command could be silently dropped**, leaving a server to be
  SIGKILLed with its world unsaved. Console attach and file exec now share one
  streaming transport (WebSocket, falling back to SPDY), instead of attach
  keeping the racier path.
- **Egg import gave no feedback.** The eggs panel scrolls internally, so a
  rejection rendered at the bottom of the card was off-screen and importing
  looked like a no-op. The result now appears next to the import controls, a
  GitHub/GitLab file-page link is rewritten to its raw form before fetching, and
  a payload that is really a web page says so instead of reporting
  `invalid character '<'`.
- **Re-enabling SFTP showed the previous, already-freed port.** Toggling now
  clears it so the panel picks up the new one.
- Ports declared by a **template** are validated like user-supplied ones; a blank
  or duplicated port name used to reach the Service, which Kubernetes rejects
  outright, leaving the server reconciling forever with no networking.
- CPU is reported in **cores** rather than millicores, and CPU and memory show
  usage against their limit with a percentage.
- The per-server **Disk** metric now reports usage of the server's **data volume**
  against the **PVC's declared size**, instead of the node filesystem. It's read
  with `du` (actual usage) rather than `df`: on local-path (hostPath-backed) PVCs
  `df` reports the whole host disk (e.g. `166/215 GiB`) instead of the server's
  volume — now it shows used vs. the PVC size (e.g. `3/10 GiB`). The `du` walk is
  cached (30s) so the 4s stats poll stays cheap; network counters still update
  every poll.
- Imported eggs now mount their data at **`/home/container`** (Pterodactyl's
  guaranteed server directory) instead of `/data`. Many eggs hardcode
  `/home/container` in their `config.files` (e.g. Terraria's `worldpath`) or
  resolve files against it — 58 of the ~250 upstream game eggs — so with the data
  volume elsewhere they wrote outside it and failed (e.g. Terraria: world save
  "Permission denied"). Aligns the data dir, `HOME`, `WorkingDir` and hardcoded
  paths exactly as on Pterodactyl. Built-in Quetzal templates keep their own
  DataPath. (Re-import existing eggs to pick up the new path.)
- `HOME` is now the server's data directory (matching Pterodactyl, where the
  container home *is* the server dir) instead of the image default
  (`/home/container`, which isn't on the data volume and is unreadable to the
  non-root user). Games and tools that resolve files via `$HOME` — notably
  SteamCMD titles looking up `~/.steam/sdk64/steamclient.so`, which the installer
  places on the data volume — now find them instead of hitting an empty/denied
  path. Set on the game, install (→ install mount), config-render, data-manager
  and SFTP containers.
- Egg startup commands now run under **bash** (falling back to sh), matching how
  Pterodactyl runs them. Many eggs use bash-only syntax — `[[ ]]` (Forge),
  process substitution and `trap`/`wait` for graceful stop and log filtering
  (Valheim and most SteamCMD eggs) — which dash (`/bin/sh`) rejected with
  "[[: not found" or "Syntax error: redirection unexpected", crashing the server
  on boot. The resolved command is passed as a positional arg, so it's parsed
  verbatim with no re-quoting.
- Imported eggs that size the JVM from `{{SERVER_MEMORY}}` (and friends) now
  start: Quetzal injects the full set of **Wings-provided globals** that eggs
  assume but never declare as variables, matching Wings' contract — `SERVER_MEMORY`
  (the memory limit in MiB), `SERVER_PORT` (the primary allocation), `SERVER_IP`
  (`0.0.0.0`), `TZ` (UTC) and `STARTUP` (the resolved invocation) — into the game,
  install and config-render containers. Previously `-Xmx{{SERVER_MEMORY}}M`
  expanded to `-Xmx M` and the JVM refused to start (affected ~25 of the official
  Minecraft eggs, e.g. Fabric, Spigot, Forge, the Technic packs and every proxy;
  Paper/Purpur were spared as they use `-XX:MaxRAMPercentage`).
- `config.files` placeholders now resolve `{{config.docker.interface}}` to the
  bind-all address (`0.0.0.0`), like `{{server.build.default.ip}}`. Wings
  substitutes its Docker bridge IP there; in Kubernetes each server has its own
  Service, so binding to all interfaces is correct. Previously the literal
  placeholder was written into the config and broke proxy binds (Waterfall,
  Travertine).
- Imported egg **install scripts that need root** now run. About half the
  official Minecraft eggs `apt-get`/`apk add` build dependencies in their
  installer image (eclipse-temurin, ghcr.io/ptero-eggs/installers), which the
  non-root runtime user can't do, so the install failed and no server jar was
  produced. The install init container now runs as root (overriding the pod's
  non-root default) and then chowns the data volume to the runtime user — the
  Wings model — which also makes the data readable by the non-root game pod on
  local-path (where `fsGroup` is a no-op).
- Signal-based stop commands are honoured. Pterodactyl encodes some stops as a
  caret token (`^C` = SIGINT, used by a few proxies/limbos); Quetzal no longer
  writes the literal `^C` to the console (a no-op) but stops the server via pod
  termination (SIGTERM + grace), which those servers handle as a clean shutdown.
- Imported eggs no longer run as **root**: a template that declares no
  securityContext (eggs don't) now defaults to a non-root uid (988, the
  yolks/Pterodactyl "container" user) with a matching fsGroup so the data volume
  stays writable. Built-in templates keep their own context.
- Imported eggs install and run correctly: their install script now (a) runs
  under a POSIX shell even when the egg export uses Windows (CRLF) line endings,
  and (b) receives the server's variables in its environment (e.g.
  `${SERVER_JARFILE}`, `${MINECRAFT_VERSION}`), as Pterodactyl runs it — without
  them an egg's installer downloaded nothing. And (c) the game container now runs
  in the data directory, so egg startup commands using relative paths (e.g.
  `java -jar server.jar`) find their files.
- Multi-node co-location for the ReadWriteOnce data volume: the data-manager now
  has a preferred affinity back to the game pod (so a data-manager-only reschedule
  returns to the volume's node), and backup Jobs co-locate with the data-manager
  (backups mount the volume while it's still held); restore Jobs run only after
  the volume is free. No effect on single-node clusters.
- Wake-on-connect: the activator pod failed to start (`container has runAsNonRoot
  and image has non-numeric user (nonroot)`) because it ran the distroless Quetzal
  image without a numeric `runAsUser`; pinned to uid 65532. Its wake callback to
  the apiserver also timed out under CNIs that enforce NetworkPolicy: the default
  policy now applies only to the untrusted workload pods (game + data-manager),
  not the Quetzal-controlled activator/backup Job, which need cluster/external
  egress the generic policy can't express.
- The per-server SFTP NodePort is now drawn from Quetzal's managed node-port pool
  (the same pool as the game ports) instead of Kubernetes' auto-assignment, so the
  two allocators can no longer pick the same port and conflict. Released back to
  the pool when SFTP is disabled or the server is deleted.
- Reject implausibly small memory limits (e.g. `4`, meaning 4 bytes for a
  missing unit) instead of producing a pod stuck on a cryptic cgroup error;
  resources are now validated on create as well as update.
- Server creation no longer fails with `variable "TYPE" is not editable` when a
  template has fixed (non-editable) variables.
- **A failed install is no longer silent.** An egg's install script runs as an
  init container, and nothing read `InitContainerStatuses` — so a script that
  exited non-zero left the pod in `Init:Error` while the panel reported
  **Starting**, indefinitely: no message, no activity entry, no notification,
  with the reason sitting in a container log nobody surfaced. Given that
  importing Pterodactyl eggs is the point, and their install scripts fail for
  ordinary reasons (a dead download URL, an apt mirror, a missing API key), this
  was the worst possible thing to be quiet about. A failing setup step now puts
  the server in **Error** with the step, its exit code and its message, emits
  `server.install-failed` so channels are told, and a setup still in progress
  reports **Installing** instead of looking stuck. The config render is covered
  the same way.
- **An exported template can be imported again.** Export writes Quetzal's own
  JSON; import only ever read the Pterodactyl egg vocabulary. The two name the
  same things differently (`env_variable` vs `envVariable`, `docker_images` vs
  `images`, `scripts.installation` vs `install`), so feeding an export to the
  import box did not fail — it **succeeded**, with `201 Created`, producing a
  template with no image, no install script and every variable's env name blank.
  The first sign came later, on creating a server: `variable "" is required`,
  naming nothing. Import now detects which of the two formats a document is in,
  so a template moves between installs by exporting and importing it. An `id` in
  the body is ignored rather than inserted over whatever row holds it here.
- **An egg whose install image lacks the interpreter it asks for now runs
  anyway.** Naming the egg's interpreter directly as the container's command
  meant an egg pairing, say, `ash` with a Debian install image died before any
  process ran — `exit 128` from runc, mentioning neither the egg nor the install,
  with an empty log. The install container now runs `/bin/sh`, which execs the
  interpreter the egg asked for, or the closest one the image has (bash, ash,
  sh), saying which in the log. A script that cannot work on that image still
  fails, but it fails on its own terms — `apk: not found` says the egg expects
  Alpine and its install image is not Alpine, which is the actual fault.
- **The setup output is readable.** New `GET /api/servers/{id}/install-log`, and
  a **Setup log** panel on the server page that opens itself while installing or
  after a failure and polls while work is in progress. It needs the console
  permission, not view: an install script runs with the server's environment,
  secret variables included. Each step carries the state Kubernetes reports for
  it, which is the whole answer when the step produced no log — a container that
  never started wrote nothing, and showing only its empty output would say "no
  install step" about an install that is sitting there failing.
- **An install that fails now fails, and runs again.** The generated install
  script ended with the marker write and a `chown … || true`, so its exit status
  was always zero — an egg's script could not fail, whatever it did — and the
  "installed" marker was written either way, so the next start skipped the
  install entirely and left the server broken with nothing to retry and nothing
  to read. The status is now taken from the script itself, and a failure leaves
  before anything is marked. What this does not fix is a script that fails
  halfway and still exits zero: egg scripts do not `set -e`, and forcing it would
  break the many that step over a command on purpose. That one shows in the
  setup log.
- **A transfer no longer tears itself down on a momentary database error.**
  Reading the backup record treated every error as "record lost", so a
  `database is locked` while SQLite was busy aborted the move — and in the
  restoring phase it rolled it back, which *deletes the destination namespace*
  and so destroyed a restore that may have completed. Only a genuinely missing
  record does that now; anything else is retried on the next tick.
- **A stalled transfer can be cancelled.** Power, edits, suspension and backups
  all answer `409` while a transfer is running, so a restic Job that stalled —
  a bucket that stopped answering, say — pinned the server for good, with no way
  out but deleting it. `DELETE /api/servers/{id}/transfer`, and a **Cancel
  transfer** button, record the request; the controller undoes the move on its
  next tick, dropping it outright before the cluster flip and rolling the
  destination back after it. The source data is untouched in both cases. The
  backup and restore Jobs also carry a six-hour deadline now, so one that hangs
  fails with a reason instead of running forever.
- **A download cut short no longer passes for a whole file.** Reading a file and
  archiving a directory ran under the 60-second limit meant for small file
  operations, so a large world download was cut mid-stream — after the 200 and
  its headers had gone out — and the error JSON was appended to the truncated
  archive. Downloads now get six hours (a client that leaves ends them sooner),
  and a failure after the first byte aborts the response so the client sees a
  broken transfer.
- **An install that hangs now ends.** An init container has no deadline of its
  own, so a script stuck on a dead mirror left the server in Installing forever.
  The install now runs under a six-hour watchdog, then fails with the reason in
  the setup log and runs again on the next start.
- **An egg's own `exit` no longer skips the install record.** The egg's script was
  inlined into the logic that records the install, so a top-level `exit 0` — or
  a bare `exit`, which is usually 0 — ended it before the marker was written and
  before the files were handed to the server's user. The install then re-ran on
  every start and the data stayed owned by root. The script now runs as its own
  process, as Wings runs it.
- **A silent connection no longer keeps a proxy-mode server awake.** The
  activator counted open TCP connections as activity, so one that carried nothing
  kept the server from hibernating for as long as the game tolerated it
  (Minecraft does not: it drops silent clients after 30 seconds). Activity is now
  traffic; nothing is disconnected for being quiet.
- **A config file is never rewritten in place.** `config.files` rendering
  truncated and rewrote each file on every start, so an eviction mid-write left it
  empty or cut short. It now writes a temporary file and renames it over the
  original, keeping its permissions.
- **A notification interrupted by a restart is delivered after it**, instead of
  being skipped.

### Security

- **Go 1.26 and current dependencies.** Go maintains the two most recent majors,
  so 1.23 had stopped receiving security fixes and the pinned build image was
  frozen on whatever it shipped with. `govulncheck` found thirteen advisories in
  code the project actually calls — ten of them in `golang.org/x/crypto/ssh`,
  which is what the SFTP server is built on: source-address restrictions not
  enforced for non-public-key auth, certificate restrictions bypassed, deadlocks
  and an underflow panic. Also an SQL injection in `pgx` (Postgres installs),
  unbounded memory in `spdystream` (the exec and attach path), a weak PRNG in
  the WebSocket masking, and an infinite loop in `x/text`. All upgraded; the
  scan now reports none, and CI runs it on every push so the next one is caught
  rather than accumulated.
- **A scheduled task now needs the permission the action itself needs.** A
  subuser holding only **schedules** could put a `command` task on a one-minute
  cron and get the console they were never granted — on a Minecraft server, that
  is `op`. Power and backup tasks went the same way. Each task in a chain is now
  checked against the matching permission (`console`, `power`, `backups`) when
  the schedule is created or edited, and a chain containing one unauthorized task
  is refused whole. Owners and admins are unaffected. Switching a schedule *off*
  needs nothing beyond **schedules**, so anyone who can manage them can stop a
  task that is misbehaving. Note that a schedule keeps running with the
  permissions it was created under: revoking a subuser's console access does not
  disable the schedules they already made, so review them when you take a
  permission away.
- **Changing a password now ends every other session.** Only the reset-by-email
  flow revoked sessions; a self-service change and an admin reset both left
  existing logins working — so the one action anyone takes after a session is
  stolen did nothing about it. Both now invalidate the account's other sessions
  while keeping the client that asked for the change signed in. API keys are
  separate credentials and are untouched: revoke them from **Account → API keys**
  if they may also be compromised.
- **Deleting a user no longer strands their servers.** The servers kept running
  with an owner id pointing at a deleted account: nobody accountable for them,
  and the owner-based resource quota silently skipped on every later edit (a
  subuser with **settings** could raise memory and CPU without limit). They are
  now reassigned to the admin performing the deletion, recorded in the audit
  entry. Because that hands over console and file access, an admin scoped to
  **users** only gets `409` when the account owns servers — reassigning them is
  a servers-level decision. A server that is already ownerless refuses resource
  changes from non-admins until an administrator reassigns it.
- **Security headers on every response.** The panel sent none: no
  `X-Content-Type-Options`, no framing protection, no CSP — while serving
  tenant-controlled file bytes from the same origin as the session cookie. All
  responses now carry `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy:
  no-referrer`, `Cross-Origin-Opener-Policy: same-origin` and a strict
  `Content-Security-Policy` (`default-src 'self'`, no inline or `eval` script,
  `frame-ancestors 'none'`). `Strict-Transport-Security` is sent only when
  `QUETZAL_SECURE_COOKIES=true`, so an http-only install is not pinned to a
  scheme it does not serve. `/api/docs` relaxes the policy for itself alone, to
  load the Redoc viewer from its CDN.
- **A two-factor code can only be used once.** A TOTP code is valid across a
  three-step window — up to 90 seconds — and nothing recorded that one had been
  used, so a code seen over a shoulder or left in a screenshot still worked
  alongside the login it came from. Each accepted code now burns its time step,
  and anything at or below the last one accepted is refused. The check and the
  write are a single statement, so two requests racing with the same code cannot
  both be let in. Recovery codes were already single-use. Consequence to know:
  the code that switches 2FA on is spent by switching it on, so the first login
  after enrolling needs the next one.
- **An admin scoped to `servers` can use SFTP.** A server's `authorized_keys`
  was built from the owner, subusers holding `files`, and superadmins only — so
  a scoped admin who administers every server could browse and edit files
  through the panel while SFTP refused their key with no explanation. They are
  now included, and an admin scoped to something else still is not.
- **A managed database only accepts its own tenants.** Nothing restricted who
  could open a connection to a Quetzal-managed MariaDB, so any pod able to route
  to its namespace could reach 3306 — other tenants' servers, and unrelated
  workloads sharing the cluster. It now carries an ingress NetworkPolicy naming
  the namespaces that hold a database on it plus the control plane's, rewritten
  on each resync as databases come and go. Per-database grants were, and remain,
  what keeps one tenant out of another's tables; this removes the chance to try.
  The policy needs `POD_NAMESPACE` to know the control plane's namespace (the
  chart sets it): without it none is written, and a warning says so, since a
  policy missing that namespace would block provisioning entirely.
- **SFTP drops a connection that never authenticates.** The SFTP server is a
  sidecar in the game server's own pod, sharing its memory limit, on a NodePort.
  A client that connected and said nothing held a goroutine and a file
  descriptor indefinitely, with no limit on how many, so silent connections from
  the internet could get the pod OOM-killed without any credentials. A
  connection now has 15 seconds to authenticate, and the number mid-handshake is
  capped; an authenticated session is not subject to the deadline.
- **The channel test button no longer returns the channel's URL.** A failed test
  delivery answered with net/http's error, which carries the request URL — for a
  Discord or webhook channel, the secret the API masks everywhere else.

## [0.1.0] - 2026-06-25

Initial public release — a Kubernetes-native control plane and web UI for hosting
game servers, with no per-node agent (Kubernetes itself runs the workloads).

### Added

- **Server lifecycle**: create from templates, power start/stop/restart/kill with
  graceful stop, editable startup variables and CPU/RAM limits, and reinstall
  (optional data wipe) guarded by an install-generation marker.
- **Console & files**: live console (log stream + `attach` stdin, no sidecar); a
  web file manager (browse/edit/upload/rename/delete, folder `.tar.gz` download,
  archive upload-and-extract) that also works while the server is stopped via an
  on-demand maintenance pod; opt-in per-server **SFTP** keyed by users' SSH keys.
- **Networking**: ClusterIP / NodePort (managed port pool) / LoadBalancer, TCP and
  UDP, real client IP by default, provider-neutral Service annotations.
- **Data & backups**: backups/restore to any S3-compatible target via restic
  (dedup, encryption, retention); keep-or-destroy data on delete; per-server
  MySQL/MariaDB provisioning against external hosts or a managed in-cluster MariaDB.
- **Automation**: cron **schedule task-chains** (power/command/backup with delays
  and continue-on-failure); **notifications** to Discord, signed webhooks, or
  email/SMTP via a durable event outbox.
- **Multi-tenant & auth**: per-server ownership and subusers, **granular admin
  roles**, admin suspend, per-user quotas, append-only audit log, API keys,
  **2FA (TOTP)** with recovery codes, and self-service password reset by email.
- **Hibernation**: scale-to-zero on idle with **wake-on-connect** in drop (TCP)
  and proxy (TCP+UDP) modes.
- **Multi-cluster**: kubeconfig-based cluster registry (encrypted), per-server
  deploy target, and **server transfer between clusters** via the backup target.
- **Egg compatibility**: import Pterodactyl/Pelican eggs (variables, startup,
  install scripts, `config.files` rendering); built-in templates for Minecraft
  (Paper and CurseForge modpacks), Valheim, and a generic process.
- **Platform**: DB-as-source-of-truth reconciler with no CRDs; SQLite or Postgres;
  leader-elected controller; OpenAPI spec + `/api/docs`; Prometheus `/metrics`;
  build-info `/api/version`; internationalized UI (English + French); version
  stamping and a tag-driven release workflow (image + Helm chart + GitHub Release).
- **Secure by default**: namespace-per-server, deny-by-default NetworkPolicy,
  hardened `securityContext`, no ServiceAccount token in game pods, per-namespace
  ResourceQuota, secrets encrypted at rest, login/2FA rate-limiting, and CSRF
  protection.

### Notes

- Licensed under **AGPL-3.0-or-later**.

[Unreleased]: https://github.com/lolozini/quetzal/compare/v0.15.0...HEAD
[0.15.0]: https://github.com/lolozini/quetzal/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/lolozini/quetzal/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/lolozini/quetzal/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/lolozini/quetzal/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/lolozini/quetzal/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/lolozini/quetzal/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/lolozini/quetzal/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/lolozini/quetzal/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/lolozini/quetzal/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/lolozini/quetzal/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/lolozini/quetzal/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/lolozini/quetzal/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/lolozini/quetzal/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/lolozini/quetzal/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/lolozini/quetzal/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/lolozini/quetzal/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/lolozini/quetzal/releases/tag/v0.1.0
