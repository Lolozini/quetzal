# Migrating from Pterodactyl

Quetzal reads Pterodactyl and Pelican eggs. A migration is two steps: bring the
eggs over once, then move each server's files.

## 1. Import the eggs

A server runs on the template made from its egg, so the egg comes first.
Templates are managed under **Admin → Eggs / templates** (the `templates` admin
permission).

### From your panel (recommended)

On the Pterodactyl panel, open **Admin → Nests → *the nest* → *the egg*** and
click **Export**. This gives you the egg exactly as your servers use it,
including any changes you made to it. In Quetzal, paste the file's content under
**Import an egg**.

### From an egg repository, by URL

The community eggs live in the [pelican-eggs](https://github.com/pelican-eggs)
organisation on GitHub (`minecraft`, `games-steamcmd`, `games-standalone`,
`generic`, `database`, `voice`, …). Each egg's folder holds two files:

- `pterodactyl-egg-<name>.json`, the Pterodactyl format;
- `egg-<name>.yaml`, Pelican's format.

Quetzal reads both. Copy the link of either file from your browser and paste it
under **Import from URL**: a GitHub or GitLab *file page* link is rewritten to
the raw file, so there is no need to look for the **Raw** button. For example,
the Paper egg:

```
https://github.com/pelican-eggs/minecraft/blob/main/java/paper/egg-paper.yaml
```

The same through the API, with an API key that has the `templates` admin
permission:

```sh
curl -X POST https://quetzal.example.com/api/templates/import-url \
  -H "Authorization: Bearer $QUETZAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://github.com/pelican-eggs/minecraft/blob/main/java/paper/egg-paper.yaml"}'
```

Things to know:

- An egg whose name matches an existing template is **not** imported over it:
  two different eggs can share a name (Pelican's and Pterodactyl's Paper).
  The panel says which template is in the way and asks whether to replace it
  or to add the egg beside it (as `paper-2`). Through the API, the import
  answers 409 and takes `?ifExists=replace` (update that template, bumping
  its version) or `?ifExists=copy`; re-importing an egg to update it needs
  `?ifExists=replace`.
- The two files of one egg are not always identical: the YAML is usually the
  newer one (it may list more images, e.g. a newer Java).
- The first image the egg lists is the template's default, as in Pterodactyl.
- The URL is fetched from the Quetzal API server, through the same guard as
  every other outbound request: an address on a private network is refused.
  For an egg that only exists on your LAN, paste its content instead.

## 2. Move each server

Quetzal does not talk to your old panel: you create the server and bring its
files over yourself.

1. **On the panel**, read the server's settings — its egg, its Docker image,
   its variables, its memory, CPU and disk limits, and its allocations.
2. **In Quetzal**, under **New server**, pick the template made from that egg
   and fill it in:

   | On Pterodactyl | In Quetzal |
   |---|---|
   | Egg | the template made from it |
   | Docker image | the same image when the template offers it, otherwise its default |
   | Variables | the template's editable variables |
   | Memory and CPU limits | memory and CPU (`4096` MB → `4096Mi`, `150` % → `1500m`) |
   | Disk limit | volume size, with room for what the files take |
   | Allocations | the ports, the default allocation as primary |

   Leave **Start immediately** off.
3. **Start it once**, so the egg's install runs and the game's own files are in
   place, then **stop it**.
4. **On the panel**, create a backup of the server and download it (a
   `.tar.gz`), or compress and download its files.
5. **In Quetzal**, on the server's **Files** tab, **Upload archive** and pick
   it, then **Extract** (2 GiB at most through the browser; above that, use
   SFTP). The archive's files replace the ones of the same name and the others
   are kept, so the install's files stay where the archive has nothing.
6. **Start it.**

The server's image needs `tar` with gzip support, since the archive is unpacked
by the data manager, which runs that image.

### What does not carry over

- **The address.** Quetzal assigns its own ports and addresses: players need the
  new one.
- Databases, subusers, schedules and existing backups. Recreate them in Quetzal.
  A database's content follows by hand: dump it on the old host
  (`mariadb-dump <database> > dump.sql`), create the server's database in
  Quetzal, upload the dump with its files, and load it with **Import SQL** on
  the server's Databases tab, server stopped.
- Variables an egg hides. Read them on the panel, or they take the template's
  default.
- **What `.pteroignore` leaves out of backups.** Quetzal does not read it:
  rename it `.quetzalignore` to leave the same paths out of the new server's
  backups.
