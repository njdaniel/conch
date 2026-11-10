import { defineConfig } from 'eslint/config';
import tseslint from 'typescript-eslint';

// tests/e2e/page imports livekit-client from web/node_modules, which the
// desktop check does not have (the desktop job installs desktop/ only). Its
// typecheck and its lint run in make desktop-voice-e2e, which builds web
// first: DESKTOP_LINT_E2E_PAGE=1 (npm run lint:e2e) lints it and nothing else.
const e2ePage = process.env.DESKTOP_LINT_E2E_PAGE === '1';

export default defineConfig(
  { ignores: ['out/', 'dist/', 'node_modules/', ...(e2ePage ? [] : ['tests/e2e/page/'])] },
  {
    files: ['**/*.ts'],
    extends: [tseslint.configs.strictTypeChecked],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      // CLAUDE.md: no `any`.
      '@typescript-eslint/no-explicit-any': 'error',
    },
  },
);
