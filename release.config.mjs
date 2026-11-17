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
            { type: 'feat', section: 'Added' },
            { type: 'fix', section: 'Fixed' },
            { type: 'build', section: 'Changed' },
            { type: 'perf', section: 'Changed' },
            { type: 'refactor', section: 'Changed' },
            { type: 'revert', section: 'Changed' },
            { type: 'chore', scope: 'deps', section: 'Dependencies' },
            { type: 'chore', effect: 'hidden' },
            { type: 'ci', effect: 'hidden' },
            { type: 'docs', effect: 'hidden' },
            { type: 'style', effect: 'hidden' },
            { type: 'test', effect: 'hidden' },
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
