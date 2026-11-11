`scalar.js` is the Scalar API Reference standalone bundle (MIT), `@scalar/api-reference` 1.73.0,
downloaded from https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.73.0/dist/browser/standalone.js
and embedded so that /docs works without internet access. To upgrade: download the new version
over this file and update the version here and in docs.go.

The file is never edited. `docs.go` serves it with one statement cut (`scalarGroupHeading`: a tag
whose children are all tags is otherwise drawn as a fixed heading instead of a row that opens and
closes with its groups inside); `TestScalarNestedTags` fails when an upgrade moved that statement.
