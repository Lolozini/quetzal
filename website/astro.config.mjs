// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLinksValidator from 'starlight-links-validator';

// The pages come from the repository's Markdown (scripts/sync-docs.mjs); only
// the home page is written here.
export default defineConfig({
	site: 'https://lolozini.github.io',
	base: '/quetzal',
	integrations: [
		starlight({
			title: 'Quetzal',
			description: 'Game servers, run by Kubernetes. A self-hosted alternative to Pterodactyl and Pelican.',
			logo: {
				light: './src/assets/quetzal-lockup.svg',
				dark: './src/assets/quetzal-lockup-dark.svg',
				replacesTitle: true,
			},
			favicon: '/favicon.svg',
			head: [
				{ tag: 'meta', attrs: { property: 'og:image', content: 'https://lolozini.github.io/quetzal/brand/social-preview-dark.png' } },
				{ tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
			],
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/Lolozini/quetzal' }],
			editLink: { baseUrl: 'https://github.com/Lolozini/quetzal/edit/main/website/' },
			customCss: ['./src/styles/brand.css'],
			sidebar: [
				{ label: 'Start here', items: ['why', 'quickstart', 'features', 'architecture'] },
				{ label: 'Guides', items: ['install', 'upgrade', 'migrating'] },
				{ label: 'Project', items: ['changelog', 'contributing', 'security', 'brand'] },
			],
			plugins: [starlightLinksValidator({ errorOnRelativeLinks: false, errorOnLocalLinks: false })],
		}),
	],
});
