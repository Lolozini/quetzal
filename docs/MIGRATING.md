# Migrating from Pterodactyl

Quetzal reads Pterodactyl and Pelican eggs, and can pull a whole server — its
settings and its files — out of a running Pterodactyl panel. A migration is two
steps: bring the eggs over once, then import each server.

## 1. Import the eggs

A server is imported onto the template made from its egg, so the egg comes first.
Templates are managed under **Admin → Templates** (the `templates` admin
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

- Importing an egg whose name matches an existing template **updates** that
  template (and bumps its version) instead of adding a second one.
- The two files of one egg are not always identical: the YAML is usually the
  newer one (it may list more images, e.g. a newer Java).
- The first image the egg lists is the template's default, as in Pterodactyl.
- The URL is fetched from the Quetzal API server, through the same guard as
  every other outbound request: an address on a private network is refused.
  For an egg that only exists on your LAN, paste its content instead.

## 2. Import a server

In **New server**, click **Import from Pterodactyl…** and give:

- **the address of the server's page** on the panel, as your browser shows it
  (`https://panel.example.com/server/1a2b3c4d`, any sub-page works);
- **a client API key** (`ptlc_…`), created on the panel under
  **Account → API Credentials** by a user who has access to the server. An
  application key (`ptla_…`) does not work: the import uses the client API,
  which any panel user can reach for their own servers. The key is used for the
  import and never stored.

**Read the server** fills the create form from it:

| From Pterodactyl | In Quetzal |
|---|---|
| Egg | the template with the same name (you can pick another) |
| Docker image | the same image, if the template offers it; otherwise the template's default |
| Variables | the values of the template's editable variables |
| Memory and CPU limits | memory and CPU limits (`4096` MB → `4096Mi`, `150` % → `1500m`) |
| Disk limit | volume size, raised if the files take more (their size × 1.5, plus 1 GiB) |
| Allocations | ports (TCP and UDP each, the default allocation as primary), for templates that declare none |

Warnings list what does not carry over: a variable the template fixes, a value
it does not accept, a variable it does not have, an image it does not offer.
Review the form, then **Create and import**.

The server is created **stopped**. In the background, the panel is asked for an
archive of the server — a **backup** when the server has a free backup slot,
otherwise a **compressed copy** of its files — which is streamed into the new
server's volume. The server is then marked installed, so the egg's install
script does not run over the imported files, and it starts if you asked for it.
The backup or archive made for the import is deleted from the panel afterwards.
Progress shows on the server's page; start, reinstall, restore and transfer
wait until it is done.

### What does not carry over

- **The address.** Quetzal assigns its own ports and addresses: players need the
  new one.
- Databases, subusers, schedules and existing backups. Recreate them in Quetzal.
- Variables the key cannot see (hidden by the egg) take the template's default.

### Requirements

- **Quetzal must reach the panel and the node.** The backup is downloaded from
  a link the panel signs, which points at the node running the server (Wings,
  usually on port 8080) or at the S3 bucket holding the backups. Both are
  fetched through the outbound guard: a panel or node on a private address is
  refused. For those, use the manual route below.
- **A compressed copy needs room on the panel side.** Without a backup slot,
  the archive is written next to the server's files before it is downloaded.
- **The key's user needs the server's permissions** to create, download and
  delete a backup (or to list, compress, download and delete files). The
  server's owner has them.
- **The server's image needs `tar` with gzip support.** The files are unpacked
  by the data manager, which runs the server's own image — the same requirement
  as **Upload archive** in the file manager.

### If the import fails

The server stays stopped and its page shows why. Fix the cause and click
**Retry the import** with the address and a key again (the key was not kept).
A retry overwrites the files the archive contains and leaves the others alone.

The same through the API: `POST /api/servers/{id}/import/pterodactyl` with
`{"url": "…", "apiKey": "ptlc_…", "start": true}`, on a stopped server. It also
imports into a server you created yourself.

### Doing it by hand

Where Quetzal cannot reach the panel, move the data yourself:

1. On the panel, create a backup of the server and download it (a `.tar.gz`).
2. In Quetzal, create the server from the egg's template with the same
   variables, **without** starting it.
3. In its **Files** tab, **Upload archive** and pick the backup (2 GiB at most
   through the browser; above that, use SFTP).
4. Start it. The egg's install runs first, as it does for any new server, then
   the server starts on your files. If the install replaces a file you want to
   keep (some installs download the server jar again), upload the archive once
   more after the install, with the server stopped.
