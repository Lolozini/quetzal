// Builds the site's pages from the repository's own Markdown, so the docs have
// one source: README.md, docs/ and the project files, as GitHub shows them.
//
// Each page gets Starlight's front matter (its title comes from the source's
// heading), links between the sources become links between pages, images are
// copied next to the site, and links to anything else point at GitHub. GitHub's
// alert blocks become Starlight asides. Everything written here is generated
// and ignored by git; hand-written pages are .mdx.

import { copyFileSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, extname, join, posix, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const site = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repo = resolve(site, '..');
const content = join(site, 'src/content/docs');
const BASE = '/quetzal';
const GITHUB = 'https://github.com/Lolozini/quetzal';

// Source (repository path) → page. `section` takes one "## " section of the file.
const PAGES = [
	{ src: 'README.md', section: 'Why Quetzal?', slug: 'why', title: 'Why Quetzal', description: 'What Quetzal is, and how it compares with Pterodactyl and Pelican.' },
	{ src: 'README.md', section: 'Quickstart', slug: 'quickstart', description: 'Install Quetzal on a Kubernetes cluster with Helm.' },
	{ src: 'README.md', section: 'Features', slug: 'features', description: 'Everything the panel does.' },
	{ src: 'README.md', section: 'How it works', slug: 'architecture', description: 'The API server, the controller, and the database between them.' },
	{ src: 'docs/INSTALL.md', slug: 'install', description: 'Install Quetzal with the Helm chart, and the choices that come with it.' },
	{ src: 'docs/UPGRADE.md', slug: 'upgrade', description: 'Move to a newer release of Quetzal.' },
	{ src: 'docs/MIGRATING.md', slug: 'migrating', description: 'Bring eggs and servers over from Pterodactyl or Pelican.' },
	{ src: 'CONTRIBUTING.md', slug: 'contributing', title: 'Contributing', description: 'Set up a development environment and send a change.' },
	{ src: 'SECURITY.md', slug: 'security', title: 'Security', description: 'Report a vulnerability, and verify the images and the chart.' },
	{ src: 'docs/brand/README.md', slug: 'brand', title: 'Brand', description: 'The logo, the colours and the type, and how to use them.' },
	{ src: 'CHANGELOG.md', slug: 'changelog', description: 'What changed in each release.' },
];
const pageOf = new Map(PAGES.filter((p) => !p.section).map((p) => [p.src, p.slug]));
pageOf.set('README.md', ''); // the whole README: the home page

// Files the site serves as they are, and where.
const ASSETS = [
	['docs/screenshots', 'public/screenshots'],
	['docs/brand', 'public/brand'],
	['web/public/fonts', 'public/fonts'],
];

function copyDir(from, to) {
	mkdirSync(to, { recursive: true });
	for (const f of readdirSync(from, { withFileTypes: true })) {
		if (f.isFile() && extname(f.name) !== '.md') copyFileSync(join(from, f.name), join(to, f.name));
	}
}

// resolveLink maps a link found in `src` (a repository path) to its place on the site.
function resolveLink(src, target) {
	if (/^([a-z]+:|#|\/\/)/i.test(target)) return target;
	const [path, hash] = target.split('#');
	const repoPath = posix.normalize(posix.join(posix.dirname(src), path));
	const anchor = hash ? `#${hash}` : '';
	if (pageOf.has(repoPath)) {
		const slug = pageOf.get(repoPath);
		return `${BASE}/${slug ? slug + '/' : ''}${anchor}`;
	}
	for (const [from] of ASSETS) {
		if (repoPath.startsWith(from + '/') && !repoPath.endsWith('.md')) {
			const to = ASSETS.find(([f]) => f === from)[1].replace(/^public/, '');
			return `${BASE}${to}/${repoPath.slice(from.length + 1)}${anchor}`;
		}
	}
	// Anything else lives in the repository.
	return `${GITHUB}/blob/main/${repoPath}${anchor}`;
}

function rewriteLinks(src, md) {
	return md
		.replace(/(\]\()([^)\s]+)(\))/g, (_, a, t, b) => a + resolveLink(src, t) + b)
		.replace(/\b(src|srcset|href)="([^"]+)"/g, (_, attr, t) => `${attr}="${resolveLink(src, t)}"`);
}

const ASIDES = { NOTE: 'note', TIP: 'tip', IMPORTANT: 'note', WARNING: 'caution', CAUTION: 'danger' };

// GitHub's "> [!WARNING]" blocks → Starlight's ":::caution" asides.
function convertAlerts(md) {
	return md.replace(/^> \[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\n((?:>.*(?:\n|$))*)/gm, (_, kind, body) => {
		const text = body.replace(/^> ?/gm, '').trimEnd();
		return `:::${ASIDES[kind]}\n${text}\n:::\n`;
	});
}

function frontMatter(fields) {
	const lines = Object.entries(fields)
		.filter(([, v]) => v)
		.map(([k, v]) => `${k}: ${JSON.stringify(v)}`);
	return `---\n${lines.join('\n')}\n---\n\n`;
}

function build(page) {
	let md = readFileSync(join(repo, page.src), 'utf8');
	let title = page.title;
	if (page.section) {
		const start = md.indexOf(`\n## ${page.section}\n`);
		if (start < 0) throw new Error(`${page.src}: no section "${page.section}"`);
		const rest = md.slice(start + 1);
		const next = rest.slice(3).search(/^## /m);
		md = next < 0 ? rest : rest.slice(0, next + 3);
		md = md.replace(/^## .*\n/, '').replace(/\n-{3,}\s*$/, '\n');
		md = md.replace(/^(#{3,6}) /gm, (_, h) => h.slice(1) + ' '); // ### → ## on its own page
		title ??= page.section;
	} else {
		const h1 = md.match(/^# (.+)\n/m);
		if (h1) {
			title ??= h1[1].trim();
			md = md.replace(h1[0], '');
		}
	}
	md = convertAlerts(rewriteLinks(page.src, md)).trimStart();
	const editUrl = `${GITHUB}/edit/main/${page.src}`;
	return frontMatter({ title, description: page.description, editUrl }) + md;
}

// Start clean: a page dropped from the list must not linger.
for (const f of readdirSync(content)) if (f.endsWith('.md')) rmSync(join(content, f));
for (const page of PAGES) writeFileSync(join(content, `${page.slug}.md`), build(page));
for (const [from, to] of ASSETS) copyDir(join(repo, from), join(site, to));
// The logo in the header, and the favicon.
mkdirSync(join(site, 'src/assets'), { recursive: true });
for (const f of ['quetzal-lockup.svg', 'quetzal-lockup-dark.svg', 'quetzal-logo.svg', 'quetzal-logo-dark.svg']) copyFileSync(join(repo, 'docs/brand', f), join(site, 'src/assets', f));
copyFileSync(join(repo, 'docs/brand/favicon.svg'), join(site, 'public/favicon.svg'));
console.log(`synced ${PAGES.length} pages into ${relative(repo, content)}`);
