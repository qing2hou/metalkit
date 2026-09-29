/// <reference types="vite/client" />

/*
 * Element Plus 的 el-table-column / el-table 插槽 row 在类型上默认是
 * DefaultRow（Record<PropertyKey, any> 的宽松别名），但它的类型收窄需要
 * 泛型经由组件库内部链路传递，unplugin-vue-components 自动引入时推断
 * 不到位，导致业务函数收到 DefaultRow 报 TS2345。
 * 这里把 TableColumnProps / TableProps 的泛型默认值统一放宽为 any，
 * 等价于 slot 作用域不做强类型检查（与 EP 官方文档用法一致：#default="{ row }"
 * 中 row 由调用方自行约定）。仅在类型层放宽，不影响运行时。
 */
import type { DefaultRow } from 'element-plus/es/components/table/src/table/defaults'
import type { TableColumnProps } from 'element-plus/es/components/table/src/table-column/defaults'

declare module 'element-plus/es/components/table/src/table-column/defaults' {
  interface TableColumnProps<T extends DefaultRow = any> {}
}
declare module 'element-plus/es/components/table/src/table/defaults' {
  interface TableProps<T extends DefaultRow = any> {}
}
