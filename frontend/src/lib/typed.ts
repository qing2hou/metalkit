/**
 * Element Plus el-table-column 的 #default 插槽 row 在类型上是 DefaultRow
 * （自动引入组件时泛型传递不到），模板里把 row 传给业务函数会报 TS2345。
 * 统一用 asRow<T>() 断言，调用点保持一行。
 */
export function asRow<T>(row: unknown): T {
  return row as T
}
