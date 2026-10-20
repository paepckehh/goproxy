const { chromium } = require("playwright");

async function main() {
  const [url, out, width, height, waitSelector, scrollSelector, fullPage] = process.argv.slice(2);
  const browser = await chromium.launch({
    executablePath: process.env.GOPROXY_CHROMIUM,
    headless: true,
    args: ["--no-sandbox", "--disable-gpu"],
  });
  const page = await browser.newPage({
    viewport: { width: Number(width), height: Number(height) },
    deviceScaleFactor: 1,
    colorScheme: "dark",
  });
  await page.goto(url, { waitUntil: "domcontentloaded" });
  if (waitSelector) {
    await page.waitForSelector(waitSelector, { timeout: 20000 });
  }
  if (scrollSelector) {
    await page.evaluate((selector) => {
      const el = document.querySelector(selector);
      if (el) el.scrollIntoView({ block: "start" });
    }, scrollSelector);
  }
  await page.waitForTimeout(1800);
  const jpeg = out.endsWith(".jpg") || out.endsWith(".jpeg");
  await page.screenshot({
    path: out,
    fullPage: fullPage === "1",
    type: jpeg ? "jpeg" : "png",
    quality: jpeg ? 88 : undefined,
  });
  await browser.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
