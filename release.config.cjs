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
        preset: 'angular',
      },
    ],
    [
      '@semantic-release/release-notes-generator',
      {
        preset: 'angular',
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
