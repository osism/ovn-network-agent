import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'ovn-network-agent',
  description: 'Event-driven network agent for OVN-based OpenStack environments',
  base: '/ovn-network-agent/',
  cleanUrls: true,
  lastUpdated: true,
  themeConfig: {
    nav: [
      { text: 'Home', link: '/' },
      { text: 'Tutorials', link: '/tutorials/first-agent' },
      { text: 'Guides', link: '/guides/installation' },
      { text: 'Reference', link: '/reference/configuration' },
      { text: 'Explanation', link: '/explanation/architecture' },
    ],
    search: {
      provider: 'local',
    },
    sidebar: [
      {
        text: 'Tutorials',
        items: [
          { text: 'First agent on a test host', link: '/tutorials/first-agent' },
        ],
      },
      {
        text: 'How-to guides',
        items: [
          { text: 'Install the agent', link: '/guides/installation' },
          { text: 'Upgrade the agent', link: '/guides/upgrade' },
          { text: 'Configure the agent', link: '/guides/configuration' },
          { text: 'Create a gatewayless provider network', link: '/guides/gatewayless-provider-network' },
          { text: 'Set up port forwarding (DNAT)', link: '/guides/port-forwarding' },
          { text: 'Configure gateway drain', link: '/guides/gateway-drain' },
          { text: 'Configure the FRR prefix list', link: '/guides/frr-prefix-list' },
          { text: 'Enable metrics and alerts', link: '/guides/metrics' },
          { text: 'Troubleshooting', link: '/guides/troubleshooting' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Configuration', link: '/reference/configuration' },
          { text: 'Command-line flags', link: '/reference/cli' },
          { text: 'Metrics', link: '/reference/metrics' },
        ],
      },
      {
        text: 'Explanation',
        items: [
          { text: 'Architecture', link: '/explanation/architecture' },
          { text: 'How the reconcile loop works', link: '/explanation/how-it-works' },
          { text: 'Multi-router support', link: '/explanation/multi-router' },
          { text: 'Gatewayless provider networks', link: '/explanation/gatewayless-networks' },
          { text: 'Port forwarding (DNAT)', link: '/explanation/port-forwarding' },
          { text: 'Gateway drain mode', link: '/explanation/gateway-drain' },
        ],
      },
      {
        text: 'Contributing',
        items: [
          { text: 'CI pipeline', link: '/contributing/ci' },
          { text: 'Integration tests', link: '/contributing/integration-tests' },
          { text: 'Containerlab E2E harness', link: '/contributing/e2e-tests' },
          { text: 'OVSDB models and OVN range', link: '/contributing/ovsdb-models' },
        ],
      },
    ],
    socialLinks: [
      { icon: 'github', link: 'https://github.com/osism/ovn-network-agent' },
    ],
    editLink: {
      pattern: 'https://github.com/osism/ovn-network-agent/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },
    footer: {
      message: 'Released under the Apache 2.0 License.',
      copyright: 'Copyright © OSISM GmbH',
    },
  },
})
