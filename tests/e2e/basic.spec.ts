import { test, expect } from '@playwright/test';

test('homepage has correct title and content', async ({ page }) => {
  await page.goto('http://localhost:3000');

  // Check the page title
  await expect(page).toHaveTitle(/Personal CRM/);

  // Check the main heading
  await expect(page.getByRole('heading', { name: 'Personal CRM' })).toBeVisible();

  // Check for key feature cards
  await expect(page.getByText('Contacts')).toBeVisible();
  await expect(page.getByText('Reminders')).toBeVisible();
  await expect(page.getByText('Notes & Interactions')).toBeVisible();
  await expect(page.getByText('AI Assistant')).toBeVisible();
});

test('backend health endpoint is accessible', async ({ request }) => {
  const response = await request.get('http://localhost:8080/health');
  
  expect(response.status()).toBe(200);
  
  const data = await response.json();
  expect(data.status).toBe('ok');
});
