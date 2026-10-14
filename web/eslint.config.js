import { defineConfig } from 'eslint/config';
import tseslint from 'typescript-eslint';

export default defineConfig(
  { ignores: ['dist/', 'node_modules/'] },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [tseslint.configs.strictTypeChecked],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      // CLAUDE.md: no `any` (strictTypeChecked already refuses it; this says so
      // here) and no dangerouslySetInnerHTML: every message body is text
      // written by someone else, and React escapes text.
      '@typescript-eslint/no-explicit-any': 'error',
      'no-restricted-syntax': [
        'error',
        {
          selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
          message: 'dangerouslySetInnerHTML is not allowed (CLAUDE.md): render text, which React escapes.',
        },
        {
          selector: "Property[key.name='dangerouslySetInnerHTML']",
          message: 'dangerouslySetInnerHTML is not allowed (CLAUDE.md): render text, which React escapes.',
        },
        {
          selector: "Property[key.value='dangerouslySetInnerHTML']",
          message: 'dangerouslySetInnerHTML is not allowed (CLAUDE.md): render text, which React escapes.',
        },
      ],
    },
  },
);
