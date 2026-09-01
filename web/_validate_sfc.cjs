// 静态校验 SFC：parse -> compileScript -> compileTemplate(inline)
// 用于 Windows 下无法启动 vite dev server 时，提前捕获模板语法/未定义变量等错误。
const fs = require('fs')
const path = require('path')
const { parse, compileScript, compileTemplate } = require('@vue/compiler-sfc')

const file = path.resolve(__dirname, 'src/App.vue')
const source = fs.readFileSync(file, 'utf-8')

const { descriptor, errors: parseErrors } = parse(source, { filename: file })
if (parseErrors.length) {
  console.error('PARSE ERRORS:', parseErrors)
  process.exit(1)
}

// script
const script = compileScript(descriptor, { id: 'app' })
if (script.errors && script.errors.length) {
  console.error('SCRIPT COMPILE ERRORS:', script.errors)
  process.exit(1)
}

// template (inline 复用编译产物中的 bindings，避免 v-model 等误报未定义)
const tpl = compileTemplate({
  source: descriptor.template.content,
  filename: file,
  id: 'app',
  compilerOptions: {
    inlineTemplate: true,
    bindingMetadata: script.bindings,
  },
})
if (tpl.errors && tpl.errors.length) {
  console.error('TEMPLATE COMPILE ERRORS:', tpl.errors)
  process.exit(1)
}

console.log('OK: SFC 编译通过（template + script 均无错误）')
