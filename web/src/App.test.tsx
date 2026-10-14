import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { App } from './App';

afterEach(cleanup);

describe('App', () => {
  it('says Conch and names the server it was served from', () => {
    render(<App origin="http://127.0.0.1:8080" />);
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Conch');
    expect(screen.getByText('http://127.0.0.1:8080')).toBeDefined();
  });
});
