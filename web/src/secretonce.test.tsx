// SecretOnce (issue #1020): a secret shown once is hidden until Reveal, and a
// share of this tab hides it, refuses Reveal, and does not bring it back when
// the share ends.
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { SecretOnce } from './admin/SecretOnce';
import { holdShare } from './voice/heldshare';

const code = 'conch_sil_SECRETCODE';

afterEach(cleanup);

describe('SecretOnce', () => {
  it('hides the secret when a share starts, refuses Reveal while it lasts and stays hidden after', async () => {
    render(<SecretOnce label="Link" value={code} onDone={() => undefined} />);
    expect(document.body.textContent).not.toContain(code);
    fireEvent.click(screen.getByRole('button', { name: 'Reveal' }));
    expect(screen.getByRole<HTMLInputElement>('textbox').value).toBe(code);

    const share = await holdShare();
    await act(async () => {});
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(document.body.innerHTML).not.toContain(code);
    expect(screen.getByRole('status').textContent).toMatch(/This tab is sharing its screen/);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Reveal' }).disabled).toBe(true);

    await act(async () => {
      await share.stop();
    });
    // Not brought back by itself.
    expect(screen.queryByRole('textbox')).toBeNull();
    expect(document.body.innerHTML).not.toContain(code);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Reveal' }).disabled).toBe(false);
  });
});
