// P3 F9c (#395): the browser says honestly that voice is not available, and
// asks for no device.
import { expect, signIn, test } from './fixtures';

test('P3-F9c the browser shows Not in voice and asks for no microphone', async ({ meet, world }) => {
  const { page } = await meet();
  // Any device request would be refused loudly: the page must not make one.
  await page.addInitScript(() => {
    const refuse = () => Promise.reject(new Error('device requested'));
    // Absent in an insecure context; the page must not reach for it either way.
    Object.defineProperty(navigator, 'mediaDevices', { value: { getUserMedia: refuse }, configurable: true });
  });
  await signIn(page, world, world.tokens.browser, 'browser-test');

  const voice = page.getByRole('region', { name: 'Voice' });
  await expect(voice).toContainText('Not in voice');
  await expect(voice).toContainText('Voice is not available in the browser yet');
  await expect(voice.getByRole('button')).toHaveCount(0);
  await expect(page.getByText(/join/i)).toHaveCount(0);
});
