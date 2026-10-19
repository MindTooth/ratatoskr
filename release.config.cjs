/** @type {import('semantic-release').GlobalConfig} */
module.exports = {
  branches: ['master'],
  tagFormat: 'v${version}',
  plugins: [
    [
      '@semantic-release/commit-analyzer',
      {
        // Keep semantic versioning correct when a custom rule also matches.
        releaseRules: [
          { breaking: true, release: 'major' },
          { type: 'build', release: 'patch' },
          { type: 'refactor', release: 'patch' },
        ],
        preset: 'conventionalcommits',
      },
    ],
    [
      '@semantic-release/release-notes-generator',
      {
        preset: 'conventionalcommits',
        presetConfig: {
          // Map release-triggering Conventional Commit types to release-note
          // categories. Non-user-facing types remain hidden.
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
