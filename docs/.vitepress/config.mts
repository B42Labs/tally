import { readFileSync } from 'node:fs'
import { defineConfig, type DefaultTheme } from 'vitepress'

// The sidebar is data, not code: docs/docs_test.go reads the same file to
// prove every page is reachable, so the two cannot disagree.
const sidebar = JSON.parse(
  readFileSync(new URL('./sidebar.json', import.meta.url), 'utf8'),
) as DefaultTheme.Sidebar

// The top navigation is data for the same reason. VitePress checks dead links
// in rendered Markdown only, so nothing but the test reads these five links,
// which sit on every page of the site.
const nav = JSON.parse(
  readFileSync(new URL('./nav.json', import.meta.url), 'utf8'),
) as DefaultTheme.NavItem[]

export default defineConfig({
  title: 'Tally',
  description: 'Reporting, metering and rating for cloud platforms',
  lang: 'en',
  // GitHub Pages serves the site under the repository path (#104, D1).
  base: '/tally/',
  // GitHub Pages serves foo.html for /foo, so links carry no extension.
  cleanUrls: true,
  lastUpdated: true,
  // The documents that predate the site. Each sibling of #104 that migrates
  // one removes its pattern here; a legacy file built before its migration
  // fails the build on its relative links, which is the intended signal.
  srcExclude: [
    'alerting.md',
    'grafana-dashboards.md',
    'group-discounts.md',
    'openstack-collector.md',
    'openstack-metrics.md',
    'openstack-reconciliation.md',
    'openstack-simulator.md',
    'drills/**',
    'runbooks/**',
  ],
  themeConfig: {
    nav,
    sidebar,
    search: { provider: 'local' },
    editLink: {
      pattern: 'https://github.com/B42Labs/tally/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },
    socialLinks: [{ icon: 'github', link: 'https://github.com/B42Labs/tally' }],
  },
})
