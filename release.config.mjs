/** @type {import('semantic-release').GlobalConfig} */
export default {
  branches: ['master'],
  tagFormat: 'v${version}',
  plugins: [
    [
      '@semantic-release/commit-analyzer',
      {
        preset: 'conventionalcommits',
      },
    ],
    [
      '@semantic-release/release-notes-generator',
      {
        preset: 'conventionalcommits',
        presetConfig: {
          // Control changelog visibility independently of release triggers.
          // Non-user-facing types remain hidden.
          types: [
            { type: 'feat', section: 'Added', hidden: false },
            { type: 'fix', section: 'Fixed', hidden: false },
            { type: 'build', section: 'Changed', hidden: false },
            { type: 'perf', section: 'Changed', hidden: false },
            { type: 'refactor', section: 'Changed', hidden: false },
            { type: 'revert', section: 'Changed', hidden: false },
            { type: 'chore', scope: 'deps', section: 'Dependencies', hidden: false },
            { type: 'chore', hidden: true },
            { type: 'ci', hidden: true },
            { type: 'docs', hidden: true },
            { type: 'style', hidden: true },
            { type: 'test', hidden: true },
          ],
        },
      },
    ],
    [
      '@semantic-release/github',
      {
        failComment: false,
        failTitle: false,
        labels: false,
        releasedLabels: false,
        successComment: false,
      },
    ],
  ],
};
