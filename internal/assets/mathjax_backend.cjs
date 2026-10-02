// Source is TeX data. Only built-in math packages and local font paths are used.
const fs = require('node:fs');
const path = require('node:path');
const readline = require('node:readline');

async function initialize(root) {
  return require(path.join(root, 'node_modules/@mathjax/src/bundle/node-main.cjs')).init({
    loader: {load: ['input/tex', 'output/svg']},
    tex: {
      // Component package arrays are additive; remove its defaults explicitly.
      packages: {'[-]': ['textmacros', 'noundefined', 'require', 'autoload', 'configmacros']},
      maxBuffer: 4096,
      maxMacros: 1000,
      formatError: (_jax, error) => {throw error;},
    },
    svg: {fontCache: 'none'},
    options: {
      compileError: (_document, _math, error) => {throw error;},
      typesetError: (_document, _math, error) => {throw error;},
    },
  });
}

async function render(mj, tex, pixels, scale) {
  tex = tex.trim();
  if (!tex || Buffer.byteLength(tex) > 4096) throw new Error('formula requires 1–4096 bytes');
  if (!Number.isInteger(scale) || scale < 1 || scale > 3) throw new Error('invalid formula raster scale');
  // Recreate parser/document state so one formula cannot define later macros.
  // The output engine and lazily loaded local font chunks stay warm.
  mj.startup.input = mj.startup.getInputJax();
  mj.startup.document = mj.startup.getDocument();
  const node = await mj.tex2svgPromise(tex, {display: true, em: pixels, ex: pixels / 2});
  const adaptor = mj.startup.adaptor;
  const svg = adaptor.firstChild(node);
  const view = adaptor.getAttribute(svg, 'viewBox').split(/\s+/).map(Number);
  // Scale the complete canvas, including padding, without changing TeX layout.
  const width = (Math.ceil(view[2] * pixels / 1000) + 4) * scale;
  const height = (Math.ceil(view[3] * pixels / 1000) + 4) * scale;
  if (!Number.isFinite(width) || !Number.isFinite(height) || width < 1 || height < 1
      || width > 4096 * scale || height > 1024 * scale || width * height > 16 * 1024 * 1024) {
    throw new Error('formula image dimensions exceed render limits');
  }
  adaptor.setAttribute(svg, 'width', String(width));
  adaptor.setAttribute(svg, 'height', String(height));
  // Padding is empty SVG space, not a larger viewport over the same viewBox.
  // Keep 1000 viewBox units per em at every raster scale.
  const canvasWidth = width / scale * 1000 / pixels;
  const canvasHeight = height / scale * 1000 / pixels;
  adaptor.setAttribute(svg, 'viewBox', [
    view[0] - (canvasWidth - view[2]) / 2,
    view[1] - (canvasHeight - view[3]) / 2,
    canvasWidth, canvasHeight,
  ].join(' '));
  adaptor.setAttribute(svg, 'color', '#e8e8e8');
  return adaptor.outerHTML(svg);
}

(async () => {
  const mj = await initialize(process.argv[1]);
  if (process.argv[2] !== 'worker') {
    process.stdout.write(await render(mj, fs.readFileSync(0, 'utf8'), Number(process.argv[2]), Number(process.argv[3])));
    return;
  }
  process.stdout.write('{"ready":true}\n');
  const input = readline.createInterface({input: process.stdin, crlfDelay: Infinity});
  for await (const line of input) {
    let reply;
    try {
      if (Buffer.byteLength(line) > 65536) throw new Error('formula request exceeds limit');
      const request = JSON.parse(line);
      const svg = await render(mj, request.tex, request.pixels, request.scale);
      reply = JSON.stringify({svg});
      if (Buffer.byteLength(reply) > 32 * 1024 * 1024) throw new Error('formula output exceeds limit');
    } catch (error) {
      reply = JSON.stringify({error: String(error.message || error).slice(0, 4096)});
    }
    process.stdout.write(reply + '\n');
  }
})().catch(error => {
  console.error(error.message || String(error));
  process.exitCode = 1;
});
