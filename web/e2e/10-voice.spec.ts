// P3 F9c (#395): the browser says honestly that voice is not available, and
// asks for no device.
import { expect, signIn, test } from './fixtures';

test('P3-F9c the browser shows Not in voice and asks for no microphone', async ({ meet, world }) => {
  const { page } = await meet();
  // Any device request is recorded and refused; the test then asserts none was made.
  await page.addInitScript(() => {
    const w = window as unknown as { deviceRequested: boolean };
    w.deviceRequested = false;
    const refuse = () => {
      w.deviceRequested = true;
      return Promise.reject(new Error('device requested'));
    };
    Object.defineProperty(navigator, 'mediaDevices', {
      value: { getUserMedia: refuse, enumerateDevices: refuse, getDisplayMedia: refuse },
      configurable: true,
    });
  });
  await signIn(page, world, world.tokens.browser, 'browser-test');

  const voice = page.getByRole('region', { name: 'Voice' });
  await expect(voice).toContainText('Not in voice');
  await expect(voice).toContainText('Voice is not available in the browser yet');
  await expect(voice.getByRole('button')).toHaveCount(0);
  await expect(page.getByText(/join/i)).toHaveCount(0);
  expect(await page.evaluate(() => (window as unknown as { deviceRequested: boolean }).deviceRequested)).toBe(false);
});
